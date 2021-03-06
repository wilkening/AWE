package core

import (
	b64 "encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/MG-RAST/AWE/lib/acl"
	"github.com/MG-RAST/AWE/lib/conf"
	"github.com/MG-RAST/AWE/lib/core/cwl"
	//cwl_types "github.com/MG-RAST/AWE/lib/core/cwl/types"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"

	"github.com/MG-RAST/AWE/lib/core/uuid"
	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/logger/event"
	"gopkg.in/mgo.v2/bson"

	"strconv"
	"strings"
	"time"
)

const (
	JOB_STAT_INIT             = "init"        // inital state
	JOB_STAT_QUEUING          = "queuing"     // transition from "init" to "queued"
	JOB_STAT_QUEUED           = "queued"      // all tasks have been added to taskmap
	JOB_STAT_INPROGRESS       = "in-progress" // a first task went into state in-progress
	JOB_STAT_COMPLETED        = "completed"
	JOB_STAT_SUSPEND          = "suspend"
	JOB_STAT_FAILED_PERMANENT = "failed-permanent" // this sepcific error state can be trigger by the workflow software
	JOB_STAT_DELETED          = "deleted"
)

var JOB_STATS_ACTIVE = []string{JOB_STAT_QUEUING, JOB_STAT_QUEUED, JOB_STAT_INPROGRESS}
var JOB_STATS_REGISTERED = []string{JOB_STAT_QUEUING, JOB_STAT_QUEUED, JOB_STAT_INPROGRESS, JOB_STAT_SUSPEND}
var JOB_STATS_TO_RECOVER = []string{JOB_STAT_INIT, JOB_STAT_QUEUING, JOB_STAT_QUEUED, JOB_STAT_INPROGRESS, JOB_STAT_SUSPEND}

type JobError struct {
	ClientFailed string `bson:"clientfailed" json:"clientfailed"`
	WorkFailed   string `bson:"workfailed" json:"workfailed"`
	TaskFailed   string `bson:"taskfailed" json:"taskfailed"`
	ServerNotes  string `bson:"servernotes" json:"servernotes"`
	WorkNotes    string `bson:"worknotes" json:"worknotes"`
	AppError     string `bson:"apperror" json:"apperror"`
	Status       string `bson:"status" json:"status"`
}

type Job struct {
	JobRaw `bson:",inline"`
	Tasks  []*Task `bson:"tasks" json:"tasks"`
}

type JobRaw struct {
	RWMutex
	Id                   string                       `bson:"id" json:"id"` // uuid
	Acl                  acl.Acl                      `bson:"acl" json:"-"`
	Info                 *Info                        `bson:"info" json:"info"`
	Script               script                       `bson:"script" json:"-"`
	State                string                       `bson:"state" json:"state"`
	Registered           bool                         `bson:"registered" json:"registered"`
	RemainTasks          int                          `bson:"remaintasks" json:"remaintasks"`
	Expiration           time.Time                    `bson:"expiration" json:"expiration"` // 0 means no expiration
	UpdateTime           time.Time                    `bson:"updatetime" json:"updatetime"`
	Error                *JobError                    `bson:"error" json:"error"`         // error struct exists when in suspended state
	Resumed              int                          `bson:"resumed" json:"resumed"`     // number of times the job has been resumed from suspension
	ShockHost            string                       `bson:"shockhost" json:"shockhost"` // this is a fall-back default if not specified at a lower level
	IsCWL                bool                         `bson:"is_cwl" json:"is_cwl`
	CwlVersion           cwl.CWLVersion               `bson:"cwl_version" json:"cwl_version"`
	CWL_objects          interface{}                  `bson:"cwl_objects" json:"cwl_objects`
	CWL_job_input        interface{}                  `bson:"cwl_job_input" json:"cwl_job_input` // has to be an array for mongo (id as key would not work)
	CWL_collection       *cwl.CWL_collection          `bson:"-" json:"-" yaml:"-" mapstructure:"-"`
	CWL_workflow         *cwl.Workflow                `bson:"-" json:"-" yaml:"-" mapstructure:"-"`
	WorkflowInstances    []interface{}                `bson:"workflow_instances" json:"workflow_instances" yaml:"workflow_instances" mapstructure:"workflow_instances"`
	WorkflowInstancesMap map[string]*WorkflowInstance `bson:"-" json:"-" yaml:"-" mapstructure:"-"`
	Entrypoint           string                       `bson:"entrypoint" json:"entrypoint"` // name of main workflow (typically has name #main or #entrypoint)
}

func (job *JobRaw) GetId(do_read_lock bool) (id string, err error) {
	if do_read_lock {
		read_lock, xerr := job.RLockNamed("String")
		if xerr != nil {
			err = xerr
			return
		}
		defer job.RUnlockNamed(read_lock)
	}

	id = job.Id
	return
}

func (job *Job) AddWorkflowInstance(id string, inputs cwl.Job_document, remain_tasks int) (err error) {
	err = job.LockNamed("AddWorkflowInstance")
	if err != nil {
		return
	}
	defer job.Unlock()

	if id == "" {
		id = "::main::"
	}

	wi := &WorkflowInstance{Id: id, Inputs: inputs, RemainTasks: remain_tasks}

	if job.WorkflowInstances == nil {
		err = fmt.Errorf("(AddWorkflowInstance) array does not exist")
		return
	}

	err = dbPushJobWorkflowInstance(job.Id, wi)
	if err != nil {
		err = fmt.Errorf("(AddWorkflowInstance) dbPushJobWorkflowInstance returned: %s", err.Error())
		return
	}

	job.WorkflowInstances = append(job.WorkflowInstances, wi)
	job.WorkflowInstancesMap[id] = wi
	return
}

func (job *Job) GetWorkflowInstanceIndex(id string, do_read_lock bool) (index int, err error) {
	if do_read_lock {
		read_lock, xerr := job.RLockNamed("GetWorkflowInstanceIndex")
		if xerr != nil {
			err = xerr
			return
		}
		defer job.RUnlockNamed(read_lock)
	}
	if id == "" {
		id = "::main::"
	}

	var wi_int interface{}

	for index, wi_int = range job.WorkflowInstances {
		var element_wi WorkflowInstance
		element_wi, err = NewWorkflowInstanceFromInterface(wi_int)
		if err != nil {
			err = fmt.Errorf("(GetWorkflowInstance) object was not a WorkflowInstance !? %s", err.Error())
			return
		}
		if element_wi.Id == id {
			//job.WorkflowInstancesMap[id] = &element_wi
			//wi = &element_wi
			return
		}

	}

	err = fmt.Errorf("(GetWorkflowInstance) WorkflowInstance %s not found", id)
	return

}

func (job *Job) GetWorkflowInstance(id string, do_read_lock bool) (wi *WorkflowInstance, err error) {
	if do_read_lock {
		read_lock, xerr := job.RLockNamed("GetWorkflowInstance")
		if xerr != nil {
			err = xerr
			return
		}
		defer job.RUnlockNamed(read_lock)
	}
	if id == "" {
		id = "::main::"
	}

	var ok bool
	wi, ok = job.WorkflowInstancesMap[id]
	if !ok {
		// for _, wi_int := range job.WorkflowInstances {
		// 			var element_wi WorkflowInstance
		// 			element_wi, err = NewWorkflowInstanceFromInterface(wi_int)
		// 			if err != nil {
		// 				err = fmt.Errorf("(GetWorkflowInstance) object was not a WorkflowInstance !? %s", err.Error())
		// 				return
		// 			}
		// 			if element_wi.Id == id {
		// 				job.WorkflowInstancesMap[id] = &element_wi
		// 				wi = &element_wi
		// 				return
		// 			}
		//
		// 		}

		err = fmt.Errorf("(GetWorkflowInstance) id \"%s\" not found", id)
		return
	}

	return
}

func (job *Job) Set_WorkflowInstance_Outputs(id string, outputs cwl.Job_document) (err error) {
	err = job.LockNamed("Set_WorkflowInstance_Outputs")
	if err != nil {
		return
	}
	defer job.Unlock()

	if id == "" {
		id = "::main::"
	}

	err = dbUpdateJobWorkflow_instancesFieldOutputs(job.Id, id, outputs)
	if err != nil {
		err = fmt.Errorf("(Set_WorkflowInstance_Outputs) dbUpdateJobWorkflow_instancesFieldOutputs returned: %s", err.Error())
		return
	}

	var index int
	index, err = job.GetWorkflowInstanceIndex(id, false)
	if err != nil {
		err = fmt.Errorf("(Set_WorkflowInstance_Outputs) GetWorkflowInstanceIndex returned: %s", err.Error())
		return
	}

	var workflow_instance WorkflowInstance
	workflow_instance_if := job.WorkflowInstances[index]
	workflow_instance, err = NewWorkflowInstanceFromInterface(workflow_instance_if)
	if err != nil {
		err = fmt.Errorf("(Set_WorkflowInstance_Outputs) NewWorkflowInstanceFromInterface returned: %s", err.Error())
		return
	}

	workflow_instance.Outputs = outputs

	job.WorkflowInstances[index] = workflow_instance
	job.WorkflowInstancesMap[id] = &workflow_instance

	return
}

func (job *Job) Decrease_WorkflowInstance_RemainTasks(id string) (remain_tasks int, err error) {
	err = job.LockNamed("Decrease_WorkflowInstance_RemainTasks")
	if err != nil {
		return
	}
	defer job.Unlock()

	if id == "" {
		id = "::main::"
	}

	var workflow_instance *WorkflowInstance
	workflow_instance, err = job.GetWorkflowInstance(id, false)
	if err != nil {
		fmt.Printf("ERROR: (Decrease_WorkflowInstance_RemainTasks) job.GetWorkflowInstance returned: %s\n", err.Error())
		err = fmt.Errorf("(Decrease_WorkflowInstance_RemainTasks) job.GetWorkflowInstance returned: %s", err.Error())
		return
	}

	remain_tasks = workflow_instance.RemainTasks - 1
	if remain_tasks < 0 {
		err = fmt.Errorf("(Decrease_WorkflowInstance_RemainTasks) new remain_tasks has invalid value (workflow_instance.RemainTasks: %d, new remain_tasks: %d)", workflow_instance.RemainTasks, remain_tasks)
		return
	}
	logger.Debug(3, "(Decrease_WorkflowInstance_RemainTasks) remain_tasks: %d", remain_tasks)

	err = dbUpdateJobWorkflow_instancesFieldInt(job.Id, id, "remaintasks", remain_tasks)
	if err != nil {
		fmt.Printf("ERROR: (Decrease_WorkflowInstance_RemainTasks) dbUpdateJobWorkflow_instancesFieldInt returned: %s\n", err.Error())
		err = fmt.Errorf("(Decrease_WorkflowInstance_RemainTasks) dbUpdateJobWorkflow_instancesFieldInt returned: %s", err.Error())
		return
	}

	// this is just to confirm the value was written TODO remove this
	var remain_tasks_mongo int
	remain_tasks_mongo, err = dbGetJobWorkflow_InstanceInt(job.Id, id, "remaintasks")
	if err != nil {
		err = fmt.Errorf("(Decrease_WorkflowInstance_RemainTasks) dbGetJobWorkflow_InstanceInt returned: %s", err.Error())
		return
	}

	if remain_tasks_mongo != remain_tasks {
		err = fmt.Errorf("(Decrease_WorkflowInstance_RemainTasks) mongo value wrong: remain_tasks_mongo: %d  , remain_tasks: %d", remain_tasks_mongo, remain_tasks)
		panic(err.Error())
		return
	}

	workflow_instance.RemainTasks = remain_tasks

	return
}

// Deprecated JobDep struct uses deprecated TaskDep struct which uses the deprecated IOmap.  Maintained for backwards compatibility.
// Jobs that cannot be parsed into the Job struct, but can be parsed into the JobDep struct will be translated to the new Job struct.
// (=deprecated=)
type JobDep struct {
	JobRaw `bson:",inline"`
	Tasks  []*TaskDep `bson:"tasks" json:"tasks"`
}

type JobMin struct {
	Id            string                 `bson:"id" json:"id"`
	Name          string                 `bson:"name" json:"name"`
	Size          int64                  `bson:"size" json:"size"`
	SubmitTime    time.Time              `bson:"submittime" json:"submittime"`
	CompletedTime time.Time              `bson:"completedtime" json:"completedtime"`
	ComputeTime   int                    `bson:"computetime" json:"computetime"`
	Task          []int                  `bson:"task" json:"task"`
	State         []string               `bson:"state" json:"state"`
	UserAttr      map[string]interface{} `bson:"userattr" json:"userattr"`
}

type JobLog struct {
	Id         string     `bson:"id" json:"id"`
	State      string     `bson:"state" json:"state"`
	UpdateTime time.Time  `bson:"updatetime" json:"updatetime"`
	Error      *JobError  `bson:"error" json:"error"`
	Resumed    int        `bson:"resumed" json:"resumed"`
	Tasks      []*TaskLog `bson:"tasks" json:"tasks"`
}

func NewJobRaw() (job *JobRaw) {
	r := &JobRaw{
		Info: NewInfo(),
		Acl:  acl.Acl{},
	}
	r.RWMutex.Init("Job")
	return r
}

func NewJob() (job *Job) {
	r_job := NewJobRaw()
	job = &Job{JobRaw: *r_job}
	return
}

func NewJobDep() (job *JobDep) {
	r_job := NewJobRaw()
	job = &JobDep{JobRaw: *r_job}
	return
}

// this has to be called after Unmarshalling from JSON
func (job *Job) Init() (changed bool, err error) {
	changed = false
	job.RWMutex.Init("Job")

	if job.State == "" {
		job.State = JOB_STAT_INIT
		changed = true
	}
	job.Registered = true

	if job.Id == "" {
		job.setId() //uuid for the job
		logger.Debug(3, "(Job.Init) Set JobID: %s", job.Id)
		changed = true
	} else {
		logger.Debug(3, "(Job.Init)  Already have JobID: %s", job.Id)
	}

	if job.Info == nil {
		logger.Error("job.Info == nil")
		job.Info = NewInfo()
	}

	if job.Info.SubmitTime.IsZero() {
		job.Info.SubmitTime = time.Now()
		changed = true
	}

	if job.Info.Priority < conf.BasePriority {
		job.Info.Priority = conf.BasePriority
		changed = true
	}

	old_remaintasks := job.RemainTasks
	job.RemainTasks = 0

	for _, task := range job.Tasks {
		if task.Id == "" {
			// suspend and create error
			logger.Error("(job.Init) task.Id empty, job %s broken?", job.Id)
			//task.Id = job.Id + "_" + uuid.New()
			task.Id = uuid.New()
			job.State = JOB_STAT_SUSPEND
			job.Error = &JobError{
				ServerNotes: "task.Id was empty",
				TaskFailed:  task.Id,
			}
			changed = true
		}
		t_changed, xerr := task.Init(job)
		if xerr != nil {
			err = fmt.Errorf("(job.Init) task.Init returned: %s", xerr.Error())
			return
		}
		if t_changed {
			changed = true
		}
		if task.State != TASK_STAT_COMPLETED {
			job.RemainTasks += 1
		}
	}

	// try to fix inconsistent state
	if job.RemainTasks != old_remaintasks {
		changed = true
	}

	// try to fix inconsistent state
	if job.RemainTasks == 0 && job.State != JOB_STAT_COMPLETED {
		job.State = JOB_STAT_COMPLETED
		logger.Debug(3, "fixing state to JOB_STAT_COMPLETED")
		changed = true
	}

	// fix job.Info.CompletedTime
	if job.State == JOB_STAT_COMPLETED && job.Info.CompletedTime.IsZero() {
		// better now, than never:
		job.Info.CompletedTime = time.Now()
		changed = true
	}

	// try to fix inconsistent state
	if job.RemainTasks > 0 && job.State == JOB_STAT_COMPLETED {
		job.State = JOB_STAT_QUEUED
		logger.Debug(3, "fixing state to JOB_STAT_QUEUED")
		changed = true
	}

	if len(job.Tasks) == 0 {
		err = errors.New("(job.Init) invalid job script: task list empty")
		return
	}

	// check that input FileName is not repeated within an individual task
	for _, task := range job.Tasks {
		inputFileNames := make(map[string]bool)
		for _, io := range task.Inputs {
			if _, exists := inputFileNames[io.FileName]; exists {
				var task_str string
				task_str, err = task.String()
				if err != nil {
					return
				}
				err = fmt.Errorf("(job.Init) invalid inputs: task %s contains multiple inputs with filename=%s", task_str, io.FileName)
				return
			}
			inputFileNames[io.FileName] = true
		}
	}

	//var workflow *cwl.Workflow

	if job.IsCWL {

		collection := cwl.NewCWL_collection()

		//var schemata_new []CWLType_Type
		named_object_array, schemata_new, xerr := cwl.NewNamed_CWL_object_array(job.CWL_objects, job.CwlVersion)
		if xerr != nil {
			err = fmt.Errorf("(job.Init) cannot type assert CWL_objects: %s", xerr.Error())
			return
		}

		err = collection.AddArray(named_object_array)
		//err = cwl.Add_to_collection(&collection, object_array)
		if err != nil {
			fmt.Errorf("(job.Init) collection.AddArray returned: %s", err.Error())
			return
		}

		err = collection.AddSchemata(schemata_new)
		if err != nil {
			err = fmt.Errorf("(job.Init) AddSchemata returned: %s", err.Error())
			return
		}

		if job.WorkflowInstances != nil {

			if len(job.WorkflowInstances) != len(job.WorkflowInstancesMap) {

				if job.WorkflowInstancesMap == nil {
					job.WorkflowInstancesMap = make(map[string]*WorkflowInstance)
				}

				for i, _ := range job.WorkflowInstances {
					wi_int := job.WorkflowInstances[i]
					var wi WorkflowInstance
					wi, err = NewWorkflowInstanceFromInterface(wi_int)
					if err != nil {
						err = fmt.Errorf("(job.Init) object is not a WorkflowInstance: %s", err.Error())
						return
					}
					job.WorkflowInstancesMap[wi.Id] = &wi
				}

				changed = true
			}

		}

		var main_input *WorkflowInstance
		main_input, err = job.GetWorkflowInstance("::main::", true) //job.WorkflowInstancesMap["#main"]
		if err != nil {
			err = fmt.Errorf("(job.Init) workflow #main not found %s", err.Error())
			return
		}
		//main_input, xerr := cwl.NewJob_documentFromNamedTypes(job.CWL_job_input)

		if xerr != nil {
			//fmt.Println("\n\njob.CWL_job_input:\n")
			//spew.Dump(job.CWL_job_input)
			err = fmt.Errorf("(job.Init) cannot create main_input: %s", xerr.Error())
			return
		}

		main_input_map := main_input.Inputs.GetMap()

		collection.Job_input_map = &main_input_map

		entrypoint := job.Entrypoint
		cwl_workflow, ok := collection.Workflows[entrypoint]
		if !ok {
			err = fmt.Errorf("(job.Init) Workflow \"%s\" not found", entrypoint)

			for key, _ := range collection.All {
				fmt.Printf("key: " + key)
			}
			return
		}

		job.CWL_collection = &collection
		job.CWL_workflow = cwl_workflow

	}

	return
}

func (job *Job) RLockRecursive() {
	for _, task := range job.Tasks {
		task.RLockAnon()
	}
}

func (job *Job) RUnlockRecursive() {
	for _, task := range job.Tasks {
		task.RUnlockAnon()
	}
}

//set job's uuid
func (job *Job) setId() {
	job.Id = uuid.New()
	return
}

type script struct {
	Name string `bson:"name" json:"name"`
	Type string `bson:"type" json:"type"`
	Path string `bson:"path" json:"-"`
}

//---Script upload (e.g. field="upload")
func (job *Job) UpdateFile(files FormFiles, field string) (err error) {
	_, isRegularUpload := files[field]
	if isRegularUpload {
		if err = job.SetFile(files[field]); err != nil {
			return err
		}
		delete(files, field)
	}
	return
}

func (job *Job) SaveToDisk() (err error) {
	var job_path string
	job_path, err = job.Path()
	if err != nil {
		err = fmt.Errorf("Save() Path error: %v", err)
		return
	}
	bsonPath := path.Join(job_path, job.Id+".bson")
	os.Remove(bsonPath)
	logger.Debug(1, "Save() bson.Marshal next: %s", job.Id)
	nbson, err := bson.Marshal(job)
	if err != nil {
		err = errors.New("error in Marshal in job.Save(), error=" + err.Error())
		return
	}
	// this is incase job path does not exist, ignored if it does
	err = job.Mkdir()
	if err != nil {
		err = errors.New("error creating dir in job.Save(), error=" + err.Error())
		return
	}
	err = ioutil.WriteFile(bsonPath, nbson, 0644)
	if err != nil {
		err = errors.New("error writing file in job.Save(), error=" + err.Error())
		return
	}
	return
}

func Deserialize_b64(encoding string, target interface{}) (err error) {
	byte_array, err := b64.StdEncoding.DecodeString(encoding)
	if err != nil {
		err = fmt.Errorf("(Deserialize_b64) DecodeString error: %s", err.Error())
		return
	}

	err = json.Unmarshal(byte_array, target)
	if err != nil {
		return
	}

	return
}

func (job *Job) Save() (err error) {
	if job.Id == "" {
		err = fmt.Errorf("(job.Save()) job id empty")
		return
	}
	logger.Debug(1, "(job.Save()) saving job: %s", job.Id)

	job.UpdateTime = time.Now()
	err = job.SaveToDisk()
	if err != nil {
		err = fmt.Errorf("(job.Save()) SaveToDisk failed: %s", err.Error())
		return
	}

	logger.Debug(1, "(job.Save()) dbUpsert next: %s", job.Id)
	//spew.Dump(job)

	err = dbUpsert(job)
	if err != nil {
		err = fmt.Errorf("(job.Save()) dbUpsert failed (job_id=%s) error=%s", job.Id, err.Error())
		return
	}
	logger.Debug(1, "(job.Save()) job saved: %s", job.Id)
	return
}

func (job *Job) Delete() (err error) {
	if err = dbDelete(bson.M{"id": job.Id}, conf.DB_COLL_JOBS); err != nil {
		return err
	}
	if err = job.Rmdir(); err != nil {
		return err
	}
	logger.Event(event.JOB_FULL_DELETE, "jobid="+job.Id)
	return
}

func (job *Job) Mkdir() (err error) {
	var path string
	path, err = job.Path()
	if err != nil {
		return
	}
	err = os.MkdirAll(path, 0777)
	if err != nil {
		err = fmt.Errorf("Could not run os.MkdirAll (path: %s) %s", path, err.Error())
		return
	}
	return
}

func (job *Job) Rmdir() (err error) {
	var path string
	path, err = job.Path()
	if err != nil {
		return
	}
	return os.RemoveAll(path)
}

func (job *Job) SetFile(file FormFile) (err error) {
	var path string
	path, err = job.FilePath()
	os.Rename(file.Path, path)
	job.Script.Name = file.Name
	return
}

//---Path functions
func (job *Job) Path() (path string, err error) {
	return getPathByJobId(job.Id)
}

func (job *Job) FilePath() (path string, err error) {
	if job.Script.Path != "" {
		path = job.Script.Path
		return
	}
	path, err = getPathByJobId(job.Id)
	if err != nil {
		return
	}
	path = path + "/" + job.Id + ".script"
	return
}

func getPathByJobId(id string) (path string, err error) {
	if len(id) < 6 {
		err = fmt.Errorf("Job-Id format wrong: \"%s\"", id)
		return
	}
	path = fmt.Sprintf("%s/%s/%s/%s/%s", conf.DATA_PATH, id[0:2], id[2:4], id[4:6], id)
	return
}

func (job *Job) GetTasks() (tasks []*Task, err error) {
	tasks = []*Task{}

	read_lock, err := job.RLockNamed("GetTasks")
	if err != nil {
		return
	}
	defer job.RUnlockNamed(read_lock)

	for _, task := range job.Tasks {
		tasks = append(tasks, task)
	}
	return
}

func (job *Job) GetState(do_lock bool) (state string, err error) {
	if do_lock {
		read_lock, xerr := job.RLockNamed("GetState")
		if xerr != nil {
			err = xerr
			return
		}
		defer job.RUnlockNamed(read_lock)
	}
	state = job.State
	return
}

//---Task functions
func (job *Job) TaskList() []*Task {
	return job.Tasks
}

func (job *Job) NumTask() int {
	return len(job.Tasks)
}

func (job *Job) AddTask(task *Task) (err error) {
	err = job.LockNamed("AddTask")
	if err != nil {
		return
	}
	defer job.Unlock()

	id := job.Id

	err = dbPushJobTask(id, task)
	if err != nil {
		return
	}
	job.Tasks = append(job.Tasks, task)
	return
}

//---Field update functions

func (job *Job) SetState(newState string, oldstates []string) (err error) {
	err = job.LockNamed("SetState")
	if err != nil {
		return
	}
	defer job.Unlock()

	job_state := job.State

	if job_state == newState {
		return
	}

	if len(oldstates) > 0 {
		matched := false
		for _, oldstate := range oldstates {
			if oldstate == job_state {
				matched = true
				break
			}
		}
		if !matched {
			oldstates_str := strings.Join(oldstates, ",")
			err = fmt.Errorf("(SetState) newState: %s, old state %s does not match one of the required ones (required: %s)", newState, job_state, oldstates_str)
			return
		}
	}

	err = dbUpdateJobFieldString(job.Id, "state", newState)
	if err != nil {
		return
	}
	job.State = newState

	// set time if completed
	switch newState {
	case JOB_STAT_COMPLETED:
		newTime := time.Now()
		err = dbUpdateJobFieldTime(job.Id, "info.completedtime", newTime)
		if err != nil {
			return
		}
		job.Info.CompletedTime = newTime
	case JOB_STAT_INPROGRESS:
		time_now := time.Now()
		jobid := job.Id
		err = job.Info.SetStartedTime(jobid, time_now)
		if err != nil {
			return
		}

	}

	// unset error if not suspended
	if (newState != JOB_STAT_SUSPEND) && (job.Error != nil) {
		err = dbUpdateJobFieldNull(job.Id, "error")
		if err != nil {
			return
		}
		job.Error = nil
	}
	return
}

func (job *Job) SetError(newError *JobError) (err error) {
	err = job.LockNamed("SetError")
	if err != nil {
		return
	}
	defer job.Unlock()
	//spew.Dump(newError)

	update_value := bson.M{"error": newError}
	err = dbUpdateJobFields(job.Id, update_value)
	if err != nil {
		return
	}
	job.Error = newError
	return
}

func (job *Job) GetRemainTasks() (remain_tasks int, err error) {
	remain_tasks = job.RemainTasks
	return
}

func (job *Job) SetRemainTasks(remain_tasks int) (err error) {
	err = job.LockNamed("SetRemainTasks")
	if err != nil {
		return
	}
	defer job.Unlock()

	if remain_tasks == job.RemainTasks {
		return
	}
	err = dbUpdateJobFieldInt(job.Id, "remaintasks", remain_tasks)
	if err != nil {
		return
	}
	job.RemainTasks = remain_tasks
	return
}

func (job *Job) IncrementRemainTasks(inc int) (err error) {
	err = job.LockNamed("IncrementRemainTasks")
	if err != nil {
		return
	}
	defer job.Unlock()

	logger.Debug(3, "(IncrementRemainTasks) called with inc=%d", inc)

	newRemainTask := job.RemainTasks + inc
	logger.Debug(3, "(IncrementRemainTasks) new value of RemainTasks: %d", newRemainTask)
	err = dbUpdateJobFieldInt(job.Id, "remaintasks", newRemainTask)
	if err != nil {
		return
	}
	job.RemainTasks = newRemainTask
	return
}

func (job *Job) IncrementResumed(inc int) (err error) {
	err = job.LockNamed("IncrementResumed")
	if err != nil {
		return
	}
	defer job.Unlock()

	newResumed := job.Resumed + inc
	err = dbUpdateJobFieldInt(job.Id, "resumed", newResumed)
	if err != nil {
		return
	}
	job.Resumed = newResumed
	return
}

func (job *Job) SetClientgroups(clientgroups string) (err error) {
	err = job.LockNamed("SetClientgroups")
	if err != nil {
		return
	}
	defer job.Unlock()

	err = dbUpdateJobFieldString(job.Id, "info.clientgroups", clientgroups)
	if err != nil {
		return
	}
	job.Info.ClientGroups = clientgroups
	return
}

func (job *Job) SetPriority(priority int) (err error) {
	err = job.LockNamed("SetPriority")
	if err != nil {
		return
	}
	defer job.Unlock()

	err = dbUpdateJobFieldInt(job.Id, "info.priority", priority)
	if err != nil {
		return
	}
	job.Info.Priority = priority
	return
}

func (job *Job) SetPipeline(pipeline string) (err error) {
	err = job.LockNamed("SetPipeline")
	if err != nil {
		return
	}
	defer job.Unlock()

	err = dbUpdateJobFieldString(job.Id, "info.pipeline", pipeline)
	if err != nil {
		return
	}
	job.Info.Pipeline = pipeline
	return
}

func (job *Job) SetDataToken(token string) (err error) {
	err = job.LockNamed("SetDataToken")
	if err != nil {
		return
	}
	defer job.Unlock()

	if job.Info.DataToken == token {
		return
	}
	// update toekn in info
	err = dbUpdateJobFieldString(job.Id, "info.token", token)
	if err != nil {
		return
	}
	job.Info.DataToken = token

	// update token in IO structs
	err = QMgr.UpdateQueueToken(job)
	if err != nil {
		return
	}

	// set using auth if not before
	if !job.Info.Auth {
		err = dbUpdateJobFieldBoolean(job.Id, "info.auth", true)
		if err != nil {
			return
		}
		job.Info.Auth = true
	}
	return
}

func (job *Job) SetExpiration(expire string) (err error) {
	err = job.LockNamed("SetExpiration")
	if err != nil {
		return
	}
	defer job.Unlock()

	parts := ExpireRegex.FindStringSubmatch(expire)
	if len(parts) == 0 {
		return errors.New("expiration format '" + expire + "' is invalid")
	}
	var expireTime time.Duration
	expireNum, _ := strconv.Atoi(parts[1])
	currTime := time.Now()

	switch parts[2] {
	case "M":
		expireTime = time.Duration(expireNum) * time.Minute
	case "H":
		expireTime = time.Duration(expireNum) * time.Hour
	case "D":
		expireTime = time.Duration(expireNum*24) * time.Hour
	}

	newExpiration := currTime.Add(expireTime)
	err = dbUpdateJobFieldTime(job.Id, "expiration", newExpiration)
	if err != nil {
		return
	}
	job.Expiration = newExpiration
	return
}

func (job *Job) GetDataToken() (token string) {
	return job.Info.DataToken
}

func (job *Job) GetPrivateEnv(taskid string) (env map[string]string, err error) {
	for _, task := range job.Tasks {
		var task_str string
		task_str, err = task.String()
		if err != nil {
			return
		}
		if taskid == task_str {
			env = task.Cmd.Environ.Private
			return
		}
	}
	return
}

func (job *Job) GetJobLogs() (jlog *JobLog, err error) {
	jlog = new(JobLog)
	jlog.Id = job.Id
	jlog.State = job.State
	jlog.UpdateTime = job.UpdateTime
	jlog.Error = job.Error
	jlog.Resumed = job.Resumed
	for _, task := range job.Tasks {
		var tl *TaskLog
		tl, err = task.GetTaskLogs()
		if err != nil {
			return
		}
		jlog.Tasks = append(jlog.Tasks, tl)
	}
	return
}

func ReloadFromDisk(path string) (err error) {
	id := filepath.Base(path)
	jobbson, err := ioutil.ReadFile(path + "/" + id + ".bson")
	if err != nil {
		return
	}
	job := NewJob()
	err = bson.Unmarshal(jobbson, &job)
	if err == nil {
		if err = dbUpsert(job); err != nil {
			return err
		}
	}
	return
}
