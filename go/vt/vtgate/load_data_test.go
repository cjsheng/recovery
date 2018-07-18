package vtgate

import (
	"fmt"
	"reflect"
	"testing"
	"vitess.io/vitess/go/vt/sqlparser"
)

func TestGetRowFromLine(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{
			`"1","a string","100.20"`,
			[]string{"1", "a string", "100.20"},
		},
		{
			`"2","a string containing a , comma","102.20"`,
			[]string{"2", "a string containing a , comma", "102.20"},
		},
		{
			`"3","a string containing a \" quote","102.20"`,
			[]string{"3", "a string containing a \" quote", "102.20"},
		},

		{
			`"4","a string containing a \", quote and comma","102.20"`,
			[]string{"4", "a string containing a \", quote and comma", "102.20"},
		},
		// Test some escape char.
		{
			`"\0\b\n\r\t\Z\\\  \c\'\""`,
			[]string{string([]byte{0, '\b', '\n', '\r', '\t', 26, '\\', ' ', ' ', 'c', '\'', '"'})},
		},
		{
			`"5","a string containing a ',single quote and comma","102.20"`,
			[]string{"5", "a string containing a \\',single quote and comma", "102.20"},
		},
		{
			`"6","a string containing a " quote","102.20"`,
			[]string{"6", "a string containing a \\\" quote", "102.20"},
		},
	}
	e := &LoadDataInfo{
		FieldsInfo: &sqlparser.FieldsClause{
			Terminated: ",",
			Enclosed:   '"',
		},
	}

	for _, test := range tests {
		if got, err := e.GetRowFromLine([]byte(test.input)); err != nil {
			t.Errorf("GetRowFromLine error: %v", err)
		} else {
			if !reflect.DeepEqual(got, test.expected) {
				t.Errorf("got: %v  but: %v", got, test.expected)
			} else {
				fmt.Printf("GOT: %v   EXPECTED: %v\n", got, test.expected)
			}
		}

	}
}
