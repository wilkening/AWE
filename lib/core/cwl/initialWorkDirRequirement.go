package cwl

import (
	"fmt"
	"reflect"

	"github.com/davecgh/go-spew/spew"
	"github.com/mitchellh/mapstructure"
)

// http://www.commonwl.org/v1.0/CommandLineTool.html#InitialWorkDirRequirement
type InitialWorkDirRequirement struct {
	BaseRequirement `bson:",inline" yaml:",inline" json:",inline" mapstructure:",squash"`
	Listing         []interface{} `yaml:"listing,omitempty" bson:"listing,omitempty" json:"listing,omitempty" mapstructure:"listing,omitempty"` // TODO: array<File | Directory | Dirent | string | Expression> | string | Expression
}

func (c InitialWorkDirRequirement) GetId() string { return "" }

func NewInitialWorkDirRequirement(original interface{}) (r *InitialWorkDirRequirement, err error) {
	var requirement InitialWorkDirRequirement
	r = &requirement

	original, err = MakeStringMap(original)
	if err != nil {
		return
	}

	switch original.(type) {

	case map[string]interface{}:

		original_map, ok := original.(map[string]interface{})
		if !ok {
			err = fmt.Errorf("(NewInitialWorkDirRequirement) type error")
			return
		}

		listing, has_listing := original_map["listing"]
		if !has_listing {
			err = fmt.Errorf("(NewInitialWorkDirRequirement) Listing is missing")
			return
		}

		original_map["listing"], err = CreateListingArray(listing)
		if err != nil {
			err = fmt.Errorf("(NewInitialWorkDirRequirement) NewCWLType returned: %s", err.Error())
			return
		}

		err = mapstructure.Decode(original, &requirement)

		requirement.Class = "InitialWorkDirRequirement"

	default:
		spew.Dump(original)
		err = fmt.Errorf("(NewInitialWorkDirRequirement) unknown type %s", reflect.TypeOf(original))
	}

	return
}

func NewListingFromInterface(original interface{}) (obj CWL_object, err error) {

	original, err = MakeStringMap(original)
	if err != nil {
		return
	}

	native_map, ok := original.(map[string]interface{})
	if ok {
		_, has_entry := native_map["entry"]
		if has_entry {
			obj, err = NewDirentFromInterface("", native_map)
			if err != nil {
				err = fmt.Errorf("(NewCWLType) NewDirent returned: %s", err.Error())
			}
			return
		}
	}
	var x CWLType
	x, err = NewCWLType("", original)
	if err != nil {
		err = fmt.Errorf("(NewListingFromInterface) NewCWLType returns: %s", err.Error())
		return
	}
	x_class := x.GetClass()
	switch x_class {
	case "File":
	case "Directory":
	case "Dirent":
	case "String":
	case "Expression":
	default:
		err = fmt.Errorf("(NewListingFromInterface) type %s is not a valid Listing (original_type: %s)", x_class, reflect.TypeOf(original))
		return
	}
	obj = x
	return

}

func CreateListingArray(original interface{}) (array []CWL_object, err error) {

	array = []CWL_object{}

	switch original.(type) {
	case []interface{}:
		original_array := original.([]interface{})

		for i, _ := range original_array {

			var new_listing CWL_object
			new_listing, err = NewListingFromInterface(original_array[i])
			if err != nil {
				err = fmt.Errorf("(CreateListingArray) NewListingFromInterface returns: %s", err.Error())
				return
			}
			array = append(array, new_listing)
		}
		return

	}

	var new_listing CWL_object
	new_listing, err = NewListingFromInterface(original)
	if err != nil {
		err = fmt.Errorf("(CreateListingArray) NewListingFromInterface returns: %s", err.Error())
		return
	}
	array = append(array, new_listing)
	return

}
