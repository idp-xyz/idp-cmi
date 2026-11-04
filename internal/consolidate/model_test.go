package consolidate

import (
	"reflect"
	"testing"
)

func TestParseStatements(t *testing.T) {
	got, err := ParseStatements("Sure! ```json\n{\"statements\": [\" Carol prefers morning pickups. \", \"\", \"Second\"]}\n```")
	if err != nil || !reflect.DeepEqual(got, []string{"Carol prefers morning pickups.", "Second"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseStatements("no json here"); err == nil {
		t.Fatal("prose accepted")
	}
	many, _ := ParseStatements(`{"statements":["a","b","c","d","e","f","g"]}`)
	if len(many) != 5 {
		t.Fatalf("%d statements", len(many))
	}
}
