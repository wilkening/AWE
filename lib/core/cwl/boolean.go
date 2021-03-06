package cwl

import (
	"fmt"
	"strconv"
)

type Boolean bool

func (b *Boolean) Is_CWL_object() {}

func (b *Boolean) GetClass() string      { return string(CWL_boolean) } // for CWL_object
func (b *Boolean) GetType() CWLType_Type { return CWL_boolean }
func (b *Boolean) String() string        { return strconv.FormatBool(bool(*b)) }

func (b *Boolean) GetId() string  { return "" }
func (b *Boolean) SetId(i string) {}

func (b *Boolean) Is_CWL_minimal() {}

func NewBooleanFrombool(value bool) (b *Boolean) {

	var b_nptr Boolean
	b_nptr = Boolean(value)

	b = &b_nptr

	return

}
func NewBoolean(id string, value bool) (b *Boolean) {

	_ = id

	return NewBooleanFrombool(value)

}

func NewBooleanFromInterface(id string, native interface{}) (b *Boolean, err error) {

	_ = id

	real_bool, ok := native.(bool)
	if !ok {
		err = fmt.Errorf("(NewBooleanFromInterface) Cannot create bool")
		return
	}
	b = NewBooleanFrombool(real_bool)
	return
}
