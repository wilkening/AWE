package core

import (
	"errors"
	"fmt"
	"github.com/MG-RAST/AWE/lib/conf"
	e "github.com/MG-RAST/AWE/lib/errors"
	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/logger/event"
	"github.com/MG-RAST/AWE/lib/user"
	"gopkg.in/mgo.v2/bson"
	"os"
	"strings"
	"time"
)

// this struct is embedded in ServerMgr
type CQMgr struct {
	//clientMapLock sync.RWMutex
	//clientMap     map[string]*Client
	clientMap    ClientMap
	workQueue    *WQueue
	suspendQueue bool
	coReq        chan CoReq  //workunit checkout request (WorkController -> qmgr.Handler)
	coAck        chan CoAck  //workunit checkout item including data and err (qmgr.Handler -> WorkController)
	feedback     chan Notice //workunit execution feedback (WorkController -> qmgr.Handler)
	coSem        chan int    //semaphore for checkout (mutual exclusion between different clients)
}

func NewCQMgr() *CQMgr {
	return &CQMgr{
		//clientMap:    map[string]*Client{},
		clientMap:    ClientMap{_map: map[string]*Client{}},
		workQueue:    NewWQueue(),
		suspendQueue: false,
		coReq:        make(chan CoReq),
		coAck:        make(chan CoAck),
		feedback:     make(chan Notice),
		coSem:        make(chan int, 1), //non-blocking buffered channel
	}
}

//--------mgr methods-------

func (qm *CQMgr) ClientHandle() {
	for {
		select {
		case coReq := <-qm.coReq:
			logger.Debug(2, fmt.Sprintf("qmgr: workunit checkout request received, Req=%v", coReq))
			var ack CoAck
			if qm.suspendQueue {
				// queue is suspended, return suspend error
				ack = CoAck{workunits: nil, err: errors.New(e.QueueSuspend)}
			} else {
				works, err := qm.popWorks(coReq)
				ack = CoAck{workunits: works, err: err}
			}
			qm.coAck <- ack
		case notice := <-qm.feedback:
			logger.Debug(2, fmt.Sprintf("qmgr: workunit feedback received, workid=%s, status=%s, clientid=%s", notice.WorkId, notice.Status, notice.ClientId))
			if err := qm.handleWorkStatusChange(notice); err != nil {
				logger.Error("handleWorkStatusChange(): " + err.Error())
			}
		}
	}
}

// show functions used in debug
func (qm *CQMgr) ShowWorkQueue() {
	logger.Debug(1, fmt.Sprintf("current queuing workunits (%d)", qm.workQueue.Len()))
	for _, id := range qm.workQueue.List() {
		logger.Debug(1, fmt.Sprintf("workid=%s", id))
	}
	return
}

//--------accessor methods-------

func (qm *CQMgr) GetClientMap() *ClientMap {
	return &qm.clientMap
}

func (qm *CQMgr) copyClient(a *Client) (b *Client) {
	b = new(Client)
	*b = *a
	return
}

func (qm *CQMgr) AddClient(client *Client, lock bool) {
	qm.clientMap.Add(client, lock)
}

func (qm *CQMgr) GetClient(id string) (client *Client, ok bool) {
	return qm.clientMap.Get(id)
}

//func (qm *CQMgr) GetAllClients() (clients []*Client) {
//	qm.clientMap.RLock()
//	defer qm.clientMap.RUnlock()
//	for _, client := range qm.clientMap {
//		copy := qm.copyClient(client)
//		clients = append(clients, copy)
//	}
//	return
//}

func (qm *CQMgr) RemoveClient(id string, lock bool) {
	qm.clientMap.Delete(id, lock)
}

func (qm *CQMgr) DeleteClient(client *Client) (err error) {
	err = qm.ClientStatusChange(client, CLIENT_STAT_DELETED, true)
	return
}

func (qm *CQMgr) DeleteClientById(id string) (err error) {
	err = qm.ClientIdStatusChange(id, CLIENT_STAT_DELETED, true)
	return
}

func (qm *CQMgr) ClientIdStatusChange(id string, new_status string, client_write_lock bool) (err error) {
	if client, ok := qm.clientMap.Get(id); ok {
		client.Set_Status(new_status, client_write_lock)
		return
	}
	return errors.New(e.ClientNotFound)
}

func (qm *CQMgr) ClientStatusChange(client *Client, new_status string, client_write_lock bool) (err error) {
	client.Set_Status(new_status, client_write_lock)
	return

}

func (qm *CQMgr) HasClient(id string) (has bool) {
	if _, ok := qm.clientMap.Get(id); ok {
		has = true
	} else {
		has = false
	}
	return
}

func (qm *CQMgr) ListClients() (ids []string) {
	//qm.clientMap.RLock()
	//defer qm.clientMap.RUnlock()
	//for id, _ := range qm.clientMap {
	//	ids = append(ids, id)
	//}
	return qm.clientMap.GetClientIds()
}

//--------client methods-------

func (qm *CQMgr) ClientChecker() {
	for {
		time.Sleep(30 * time.Second)
		logger.Debug(3, "time to update client list....")

		delete_clients := []string{}

		qm.clientMap.RLock()
		for _, client := range *qm.clientMap.GetMap() {
			client.RLock()
			if client.Tag == true {
				client.Tag = false
				total_minutes := int(time.Now().Sub(client.RegTime).Minutes())
				hours := total_minutes / 60
				minutes := total_minutes % 60
				client.Serve_time = fmt.Sprintf("%dh%dm", hours, minutes)
				if client.Current_work_length(false) > 0 {
					client.Idle_time = 0
				} else {
					client.Idle_time += 30
				}
				//qm.AddClient(client)
			} else {
				if ok := qm.HasClient(client.Id); !ok {
					continue
				}
				//now client must be gone as tag set to false 30 seconds ago and no heartbeat received thereafter
				logger.Event(event.CLIENT_UNREGISTER, "clientid="+client.Id+";name="+client.Name)
				//requeue unfinished workunits associated with the failed client
				qm.ReQueueWorkunitByClient(client.Id)
				//delete the client from client map
				//qm.RemoveClient(client.Id)
				delete_clients = append(delete_clients, client.Id)
			}
			client.RUnlock()
		}
		qm.clientMap.RUnlock()

		// Now delete clients
		if len(delete_clients) > 0 {
			qm.clientMap.Lock()
			for _, client_id := range delete_clients {
				qm.RemoveClient(client_id, false)
			}
			qm.clientMap.Unlock()
		}
	}
}

func (qm *CQMgr) ClientHeartBeat(id string, cg *ClientGroup) (hbmsg HBmsg, err error) {
	hbmsg = make(map[string]string, 1)
	if client, ok := qm.GetClient(id); ok {
		client.Lock()
		defer client.Unlock()

		// If the name of the clientgroup (from auth token) does not match the name in the client retrieved, throw an error
		if cg != nil && client.Group != cg.Name {
			return nil, errors.New(e.ClientGroupBadName)
		}
		client.Tag = true

		logger.Debug(3, "HeartBeatFrom:"+"clientid="+id+",name="+client.Name)

		//get suspended workunit that need the client to discard
		workids := qm.getWorkByClient(id)
		suspended := []string{}

		for _, work := range qm.workQueue.GetSet(workids) {
			if work.State == WORK_STAT_SUSPEND {
				suspended = append(suspended, work.Id)
			}
		}
		if len(suspended) > 0 {
			hbmsg["discard"] = strings.Join(suspended, ",")
		}
		if client.Status == CLIENT_STAT_DELETED {
			hbmsg["stop"] = id
		}

		//hbmsg["discard"] = strings.Join(workids, ",")
		return hbmsg, nil
	}
	return hbmsg, errors.New(e.ClientNotFound)
}

func (qm *CQMgr) RegisterNewClient(files FormFiles, cg *ClientGroup) (client *Client, err error) {
	if _, ok := files["profile"]; ok {
		client, err = NewProfileClient(files["profile"].Path)
		os.Remove(files["profile"].Path)
	} else {
		client = NewClient()
	}

	client.Lock()
	defer client.Unlock()

	if err != nil {
		return nil, err
	}
	// If clientgroup is nil at this point, create a publicly owned clientgroup, with the provided group name (if one doesn't exist with the same name)
	if cg == nil {
		// See if clientgroup already exists with this name
		// If it does and it does not have "public" execution rights, throw error
		// If it doesn't, create one owned by public, and continue with client registration
		// Otherwise proceed with client registration.
		cg, _ = LoadClientGroupByName(client.Group)

		if cg != nil {
			rights := cg.Acl.Check("public")
			if rights["execute"] == false {
				return nil, errors.New("Clientgroup with the group specified by your client exists, but you cannot register with it, without clientgroup token.")
			}
		} else {
			u := &user.User{Uuid: "public"}
			cg, err = CreateClientGroup(client.Group, u)
			if err != nil {
				return nil, err
			}
		}
	}
	// If the name of the clientgroup (from auth token) does not match the name in the client profile, throw an error
	if cg != nil && client.Group != cg.Name {
		return nil, errors.New(e.ClientGroupBadName)
	}
	qm.AddClient(client, true)

	if client.Current_work_length(false) > 0 { //re-registered client
		// move already checked-out workunit from waiting queue (workMap) to checked-out list (coWorkMap)

		for workid, _ := range client.Current_work {
			if qm.workQueue.Has(workid) {
				qm.workQueue.StatusChange(workid, WORK_STAT_CHECKOUT)
			}
		}

	}
	return
}

func (qm *CQMgr) GetClientByUser(id string, u *user.User) (client *Client, err error) {
	// Get all clientgroups that user owns or that are publicly owned, or all if user is admin
	q := bson.M{}
	clientgroups := new(ClientGroups)
	dbFindClientGroups(q, clientgroups)
	filtered_clientgroups := map[string]bool{}
	for _, cg := range *clientgroups {
		if (u.Uuid != "public" && (cg.Acl.Owner == u.Uuid || u.Admin == true || cg.Acl.Owner == "public")) ||
			(u.Uuid == "public" && conf.CLIENT_AUTH_REQ == false && cg.Acl.Owner == "public") {
			filtered_clientgroups[cg.Name] = true
		}
	}

	if client, ok := qm.GetClient(id); ok {
		if val, exists := filtered_clientgroups[client.Group]; exists == true || val == true {
			return client, nil
		}
	}
	return nil, errors.New(e.ClientNotFound)
}

func (qm *CQMgr) GetAllClientsByUser(u *user.User) (clients []*Client) {
	// Get all clientgroups that user owns or that are publicly owned, or all if user is admin
	q := bson.M{}
	clientgroups := new(ClientGroups)
	dbFindClientGroups(q, clientgroups)
	filtered_clientgroups := map[string]bool{}
	for _, cg := range *clientgroups {
		rights := cg.Acl.Check(u.Uuid)
		if (u.Uuid != "public" && (cg.Acl.Owner == u.Uuid || rights["read"] == true || u.Admin == true || cg.Acl.Owner == "public")) ||
			(u.Uuid == "public" && conf.CLIENT_AUTH_REQ == false && cg.Acl.Owner == "public") {
			filtered_clientgroups[cg.Name] = true
		}
	}

	qm.clientMap.RLock()
	for _, client := range *qm.clientMap.GetMap() {
		if val, exists := filtered_clientgroups[client.Group]; exists == true && val == true {
			clients = append(clients, client)
		}
	}
	qm.clientMap.RUnlock()

	return clients
}

func (qm *CQMgr) DeleteClientByUser(id string, u *user.User) (err error) {
	// Get all clientgroups that user owns or that are publicly owned, or all if user is admin
	q := bson.M{}
	clientgroups := new(ClientGroups)
	dbFindClientGroups(q, clientgroups)
	filtered_clientgroups := map[string]bool{}
	for _, cg := range *clientgroups {
		if (u.Uuid != "public" && (cg.Acl.Owner == u.Uuid || u.Admin == true || cg.Acl.Owner == "public")) ||
			(u.Uuid == "public" && conf.CLIENT_AUTH_REQ == false && cg.Acl.Owner == "public") {
			filtered_clientgroups[cg.Name] = true
		}
	}

	if client, ok := qm.GetClient(id); ok {
		if val, exists := filtered_clientgroups[client.Group]; exists == true && val == true {
			err = qm.DeleteClient(client)
			return
		}
		return errors.New(e.UnAuth)
	}
	return errors.New(e.ClientNotFound)
}

// use id OR client
func (qm *CQMgr) SuspendClient(id string, client *Client, client_write_lock bool) (err error) {

	if client == nil {
		var ok bool
		client, ok = qm.GetClient(id)
		if !ok {
			return errors.New(e.ClientNotFound)
		}
	}

	if client_write_lock {
		client.Lock()
		defer client.Unlock()
	}

	status := client.Get_Status(false)
	if status == CLIENT_STAT_ACTIVE_IDLE || status == CLIENT_STAT_ACTIVE_BUSY {
		client.Set_Status(CLIENT_STAT_SUSPEND, false)
		//if err = qm.ClientStatusChange(id, CLIENT_STAT_SUSPEND); err != nil {
		//	return
		//}
		qm.ReQueueWorkunitByClient(id)
		return
	}
	return errors.New(e.ClientNotActive)

}

func (qm *CQMgr) SuspendAllClients() (count int) {
	for _, id := range qm.ListClients() {
		if err := qm.SuspendClient(id, nil, true); err == nil {
			count += 1
		}
	}
	return count
}

func (qm *CQMgr) SuspendClientByUser(id string, u *user.User) (err error) {
	// Get all clientgroups that user owns or that are publicly owned, or all if user is admin
	q := bson.M{}
	clientgroups := new(ClientGroups)
	dbFindClientGroups(q, clientgroups)
	filtered_clientgroups := map[string]bool{}
	for _, cg := range *clientgroups {
		if (u.Uuid != "public" && (cg.Acl.Owner == u.Uuid || u.Admin == true || cg.Acl.Owner == "public")) ||
			(u.Uuid == "public" && conf.CLIENT_AUTH_REQ == false && cg.Acl.Owner == "public") {
			filtered_clientgroups[cg.Name] = true
		}
	}

	if client, ok := qm.GetClient(id); ok {
		if val, exists := filtered_clientgroups[client.Group]; exists == true && val == true {
			client.Lock()
			defer client.Unlock()
			status := client.Get_Status(false)
			if status == CLIENT_STAT_ACTIVE_IDLE || status == CLIENT_STAT_ACTIVE_BUSY {
				client.Set_Status(CLIENT_STAT_SUSPEND, false)
				//if err = qm.ClientStatusChange(id, CLIENT_STAT_SUSPEND); err != nil {
				//	return
				//}
				qm.ReQueueWorkunitByClient(id)
				return
			}
			return errors.New(e.ClientNotActive)
		}
		return errors.New(e.UnAuth)
	}
	return errors.New(e.ClientNotFound)
}

func (qm *CQMgr) SuspendAllClientsByUser(u *user.User) (count int) {
	// Get all clientgroups that user owns or that are publicly owned, or all if user is admin
	q := bson.M{}
	clientgroups := new(ClientGroups)
	dbFindClientGroups(q, clientgroups)
	filtered_clientgroups := map[string]bool{}
	for _, cg := range *clientgroups {
		if (u.Uuid != "public" && (cg.Acl.Owner == u.Uuid || u.Admin == true || cg.Acl.Owner == "public")) ||
			(u.Uuid == "public" && conf.CLIENT_AUTH_REQ == false && cg.Acl.Owner == "public") {
			filtered_clientgroups[cg.Name] = true
		}
	}

	qm.clientMap.RLock()
	for _, client := range *qm.clientMap.GetMap() {
		client.Lock()
		status := client.Get_Status(false)
		if val, exists := filtered_clientgroups[client.Group]; exists == true && val == true && (status == CLIENT_STAT_ACTIVE_IDLE || status == CLIENT_STAT_ACTIVE_BUSY) {
			qm.SuspendClient("", client, false)
			count += 1
		}
		client.Unlock()
	}
	qm.clientMap.RUnlock()

	return count
}

func (qm *CQMgr) ResumeClient(id string) (err error) {
	if client, ok := qm.GetClient(id); ok {
		client.Lock()
		defer client.Unlock()
		if client.Status == CLIENT_STAT_SUSPEND {
			//err = qm.ClientStatusChange(id, CLIENT_STAT_ACTIVE_IDLE)
			client.Status = CLIENT_STAT_ACTIVE_IDLE
			return
		}
		return errors.New(e.ClientNotSuspended)
	}
	return errors.New(e.ClientNotFound)
}

func (qm *CQMgr) ResumeClientByUser(id string, u *user.User) (err error) {
	// Get all clientgroups that user owns or that are publicly owned, or all if user is admin
	q := bson.M{}
	clientgroups := new(ClientGroups)
	dbFindClientGroups(q, clientgroups)
	filtered_clientgroups := map[string]bool{}
	for _, cg := range *clientgroups {
		if (u.Uuid != "public" && (cg.Acl.Owner == u.Uuid || u.Admin == true || cg.Acl.Owner == "public")) ||
			(u.Uuid == "public" && conf.CLIENT_AUTH_REQ == false && cg.Acl.Owner == "public") {
			filtered_clientgroups[cg.Name] = true
		}
	}

	if client, ok := qm.GetClient(id); ok {
		client.Lock()
		defer client.Unlock()

		if val, exists := filtered_clientgroups[client.Group]; exists == true && val == true {
			if client.Status == CLIENT_STAT_SUSPEND {
				//err = qm.ClientStatusChange(id, CLIENT_STAT_ACTIVE_IDLE)
				client.Status = CLIENT_STAT_ACTIVE_IDLE
				return
			}
			return errors.New(e.ClientNotSuspended)
		}
		return errors.New(e.UnAuth)
	}
	return errors.New(e.ClientNotFound)
}

func (qm *CQMgr) ResumeSuspendedClients() (count int) {
	qm.clientMap.RLock()
	for _, client := range *qm.clientMap.GetMap() {
		client.Lock()
		if client.Status == CLIENT_STAT_SUSPEND {
			//qm.ClientStatusChange(client.Id, CLIENT_STAT_ACTIVE_IDLE)
			client.Status = CLIENT_STAT_ACTIVE_IDLE
			count += 1
		}
		client.Unlock()
	}
	qm.clientMap.RUnlock()
	return count
}

func (qm *CQMgr) ResumeSuspendedClientsByUser(u *user.User) (count int) {
	// Get all clientgroups that user owns or that are publicly owned, or all if user is admin
	q := bson.M{}
	clientgroups := new(ClientGroups)
	dbFindClientGroups(q, clientgroups)
	filtered_clientgroups := map[string]bool{}
	for _, cg := range *clientgroups {
		if (u.Uuid != "public" && (cg.Acl.Owner == u.Uuid || u.Admin == true || cg.Acl.Owner == "public")) ||
			(u.Uuid == "public" && conf.CLIENT_AUTH_REQ == false && cg.Acl.Owner == "public") {
			filtered_clientgroups[cg.Name] = true
		}
	}

	qm.clientMap.RLock()
	for _, client := range *qm.clientMap.GetMap() {
		client.Lock()
		if val, exists := filtered_clientgroups[client.Group]; exists == true && val == true && client.Status == CLIENT_STAT_SUSPEND {
			//qm.ClientStatusChange(client.Id, CLIENT_STAT_ACTIVE_IDLE)
			client.Status = CLIENT_STAT_ACTIVE_IDLE
			count += 1
		}
		client.Unlock()
	}
	qm.clientMap.RUnlock()

	return count
}

func (qm *CQMgr) UpdateSubClients(id string, count int) {
	if client, ok := qm.GetClient(id); ok {
		client.SubClients = count

	}
}

func (qm *CQMgr) UpdateSubClientsByUser(id string, count int, u *user.User) {
	// Get all clientgroups that user owns or that are publicly owned, or all if user is admin
	q := bson.M{}
	clientgroups := new(ClientGroups)
	dbFindClientGroups(q, clientgroups)
	filtered_clientgroups := map[string]bool{}
	for _, cg := range *clientgroups {
		if (u.Uuid != "public" && (cg.Acl.Owner == u.Uuid || u.Admin == true || cg.Acl.Owner == "public")) ||
			(u.Uuid == "public" && conf.CLIENT_AUTH_REQ == false && cg.Acl.Owner == "public") {
			filtered_clientgroups[cg.Name] = true
		}
	}

	if client, ok := qm.GetClient(id); ok {
		if val, exists := filtered_clientgroups[client.Group]; exists == true && val == true {
			client.SubClients = count

		}
	}
}

//--------end client methods-------

//-------start of workunit methods---

func (qm *CQMgr) CheckoutWorkunits(req_policy string, client_id string, available_bytes int64, num int) (workunits []*Workunit, err error) {
	//precheck if the client is registered
	client, hasClient := qm.GetClient(client_id)
	if !hasClient {
		return nil, errors.New(e.ClientNotFound)
	}

	// getting global lock on the client. Make sure you are not using locking client functions here
	client.Lock()
	defer client.Unlock()

	status := client.Status
	if status == CLIENT_STAT_SUSPEND {
		return nil, errors.New(e.ClientSuspended)
	}
	if status == CLIENT_STAT_DELETED {
		qm.RemoveClient(client_id, false)
		return nil, errors.New(e.ClientDeleted)
	}

	logger.Debug(3, fmt.Sprintf("lock semaphore in CheckoutWorkunits() for client: %s", client_id))
	//lock semaphore, at one time only one client's checkout request can be served
	qm.LockSemaphore()
	defer qm.UnlockSemaphore()

	req := CoReq{policy: req_policy, fromclient: client_id, available: available_bytes, count: num}
	qm.coReq <- req
	ack := <-qm.coAck

	if ack.err == nil {
		for _, work := range ack.workunits {
			client.Add_work_nolock(work.Id)
		}
		if client.Status == CLIENT_STAT_ACTIVE_IDLE {
			client.Status = CLIENT_STAT_ACTIVE_BUSY
		}
	}

	logger.Debug(3, fmt.Sprintf("unlock semaphore in CheckoutWorkunits() for client: %s", client_id))
	return ack.workunits, ack.err
}

func (qm *CQMgr) LockSemaphore() {
	qm.coSem <- 1
}

func (qm *CQMgr) UnlockSemaphore() {
	<-qm.coSem
}

func (qm *CQMgr) GetWorkById(id string) (workunit *Workunit, err error) {
	workunit, ok := qm.workQueue.Get(id)
	if !ok {
		err = errors.New(fmt.Sprintf("no workunit found with id %s", id))
	}
	return
}

func (qm *CQMgr) NotifyWorkStatus(notice Notice) {
	qm.feedback <- notice
	return
}

func (qm *CQMgr) popWorks(req CoReq) (works []*Workunit, err error) {
	logger.Debug(3, fmt.Sprintf("starting popWorks() for client: %s", req.fromclient))

	filtered, err := qm.filterWorkByClient(req.fromclient)
	if err != nil {
		return
	}
	logger.Debug(2, fmt.Sprintf("popWorks filtered: %d (0 meansNoEligibleWorkunitFound)", filtered))
	if len(filtered) == 0 {
		return nil, errors.New(e.NoEligibleWorkunitFound)
	}
	works, err = qm.workQueue.selectWorkunits(filtered, req.policy, req.available, req.count)
	if err == nil { //get workunits successfully, put them into coWorkMap
		for _, work := range works {
			work.Client = req.fromclient
			work.CheckoutTime = time.Now()
			qm.workQueue.Put(work)
			qm.workQueue.StatusChange(work.Id, WORK_STAT_CHECKOUT)
		}
	}
	logger.Debug(3, fmt.Sprintf("done with popWorks() for client: %s", req.fromclient))
	return
}

func (qm *CQMgr) filterWorkByClient(clientid string) (ids []string, err error) {
	logger.Debug(3, fmt.Sprintf("starting filterWorkByClient() for client: %s", clientid))

	client, ok := qm.GetClient(clientid)
	if !ok {
		err_msg := fmt.Sprintf("error: unregistered client %s trying to checkout workunit, most likely cause is client disappeared after request to checkout workunit combined with slow response to workunit checkout request", clientid)
		fmt.Fprintln(os.Stderr, err_msg)
		logger.Error(err_msg)
		return nil, errors.New(e.ClientNotFound)
	}

	for _, id := range qm.workQueue.WaitList() {
		work, ok := qm.workQueue.Get(id)
		if !ok {
			logger.Error(fmt.Sprintf("error: workunit %s is in wait queue but not in workMap", id))
			continue
		}
		//skip works that are in the client's skip-list
		if client.Contains_Skip_work(work.Id) {
			logger.Debug(2, fmt.Sprintf("2) client.Contains_Skip_work(work.Id) %s", id))
			continue
		}
		//skip works that have dedicate client groups which this client doesn't belong to
		if len(work.Info.ClientGroups) > 0 {
			eligible_groups := strings.Split(work.Info.ClientGroups, ",")
			if !contains(eligible_groups, client.Group) {
				logger.Debug(2, fmt.Sprintf("3) !contains(eligible_groups, client.Group) %s", id))
				continue
			}
		}
		//append works whos apps are supported by the client
		if contains(client.Apps, work.Cmd.Name) || contains(client.Apps, conf.ALL_APP) {
			ids = append(ids, id)
		} else {
			logger.Debug(2, fmt.Sprintf("3) contains(client.Apps, work.Cmd.Name) || contains(client.Apps, conf.ALL_APP) %s", id))
		}
	}
	logger.Debug(3, fmt.Sprintf("done with filterWorkByClient() for client: %s", clientid))

	return ids, nil
}

func (qm *CQMgr) getWorkByClient(clientid string) (ids []string) {
	if client, ok := qm.GetClient(clientid); ok {
		client.Lock()
		for id, _ := range client.Current_work {
			ids = append(ids, id)
		}
		client.Unlock()
	}
	return
}

//handle feedback from a client about the execution of a workunit
func (qm *CQMgr) handleWorkStatusChange(notice Notice) (err error) {
	//to be implemented for proxy or server
	return
}

func (qm *CQMgr) FetchDataToken(workid string, clientid string) (token string, err error) {
	//to be implemented for proxy or server
	return
}

func (qm *CQMgr) ShowWorkunits(status string) (workunits []*Workunit) {
	for _, work := range qm.workQueue.GetAll() {
		if work.State == status || status == "" {
			workunits = append(workunits, work)
		}
	}
	return workunits
}

func (qm *CQMgr) ShowWorkunitsByUser(status string, u *user.User) (workunits []*Workunit) {
	// Only returns workunits of jobs that the user has read access to or is the owner of.  If user is admin, return all.
	for _, work := range qm.workQueue.GetAll() {
		// skip loading jobs from db if user is admin
		if u.Admin == true {
			if work.State == status || status == "" {
				workunits = append(workunits, work)
			}
		} else {
			if jobid, err := GetJobIdByWorkId(work.Id); err == nil {
				if job, err := LoadJob(jobid); err == nil {
					rights := job.Acl.Check(u.Uuid)
					if job.Acl.Owner == u.Uuid || rights["read"] == true {
						if work.State == status || status == "" {
							workunits = append(workunits, work)
						}
					}
				}
			}
		}
	}
	return workunits
}

func (qm *CQMgr) EnqueueWorkunit(work *Workunit) (err error) {
	err = qm.workQueue.Add(work)
	return
}

func (qm *CQMgr) ReQueueWorkunitByClient(clientid string) (err error) {
	workids := qm.getWorkByClient(clientid)
	for _, workid := range workids {
		if qm.workQueue.Has(workid) {
			jobid, _ := GetJobIdByWorkId(workid)
			if job, err := LoadJob(jobid); err == nil {
				if contains(JOB_STATS_ACTIVE, job.State) { //only requeue workunits belonging to active jobs (rule out suspended jobs)
					qm.workQueue.StatusChange(workid, WORK_STAT_QUEUED)
					logger.Event(event.WORK_REQUEUE, "workid="+workid)
				}
			}
		}
	}
	return
}

//---end of workunit methods
