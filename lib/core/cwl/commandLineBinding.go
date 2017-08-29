package cwl

import (
	"fmt"
	//"github.com/davecgh/go-spew/spew"
	cwl_types "github.com/MG-RAST/AWE/lib/core/cwl/types"
	"github.com/mitchellh/mapstructure"
	"reflect"
)

//http://www.commonwl.org/v1.0/Workflow.html#CommandLineBinding
type CommandLineBinding struct {
	LoadContents  bool                  `yaml:"loadContents,omitempty" bson:"loadContents,omitempty" json:"loadContents,omitempty"`
	Position      int                   `yaml:"position,omitempty" bson:"position,omitempty" json:"position,omitempty"`
	Prefix        string                `yaml:"prefix,omitempty" bson:"prefix,omitempty" json:"prefix,omitempty"`
	Separate      bool                  `yaml:"separate,omitempty" bson:"separate,omitempty" json:"separate,omitempty"`
	ItemSeparator string                `yaml:"itemSeparator,omitempty" bson:"itemSeparator,omitempty" json:"itemSeparator,omitempty"`
	ValueFrom     *cwl_types.Expression `yaml:"valueFrom,omitempty" bson:"valueFrom,omitempty" json:"valueFrom,omitempty"`
	ShellQuote    bool                  `yaml:"shellQuote,omitempty" bson:"shellQuote,omitempty" json:"shellQuote,omitempty"`
}

func NewCommandLineBinding(original interface{}) (clb *CommandLineBinding, err error) {

	var commandlinebinding CommandLineBinding

	original, err = makeStringMap(original)
	if err != nil {
		return
	}

	switch original.(type) {
	case map[string]interface{}:
		original_map, ok := original.(map[string]interface{})
		if !ok {
			err = fmt.Errorf("(NewCommandLineBinding) type assertion error map[string]interface{}")
			return
		}

		value_from, ok := original_map["valueFrom"]
		if ok {
			exp, xerr := cwl_types.NewExpression(value_from)
			if xerr != nil {
				err = fmt.Errorf("(NewCommandLineBinding) NewExpression failed: %s", xerr.Error())
				return
			}
			original_map["valueFrom"] = *exp
		}

		err = mapstructure.Decode(original, &commandlinebinding)
		if err != nil {
			err = fmt.Errorf("(NewCommandLineBinding) mapstructure: %s", err.Error())
			return
		}
		clb = &commandlinebinding

	case string:

		org_str, _ := original.(string)

		commandlinebinding.ValueFrom, err = cwl_types.NewExpression(org_str)
		if err != nil {
			return
		}

	default:
		err = fmt.Errorf("(NewCommandLineBinding) type %s unknown", reflect.TypeOf(original))
		return
	}

	return
}

func NewCommandLineBindingArray(original interface{}) (new_array []*CommandLineBinding, err error) {
	switch original.(type) {
	case []interface{}:
		for _, v := range original.([]interface{}) {

			clb, xerr := NewCommandLineBinding(v)
			if xerr != nil {
				err = fmt.Errorf("(NewCommandLineBindingArray) []interface{} NewCommandLineBinding returned: %s", xerr)
				return
			}

			//fmt.Printf("C")
			new_array = append(new_array, clb)
			//fmt.Printf("D")

		}
		return
	case string:

		clb, xerr := NewCommandLineBinding(original)
		if xerr != nil {
			err = fmt.Errorf("(NewCommandLineBindingArray) string NewCommandLineBinding returned: %s", xerr)
			return
		}

		new_array = append(new_array, clb)

	default:
		err = fmt.Errorf("(NewCommandLineBindingArray) type unknown")
		return
	}
	return
}
