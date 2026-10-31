package isureal

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestFastJSONCompatibility(t *testing.T) {
	serializers := []echo.JSONSerializer{echo.DefaultJSONSerializer{}, fastJSONSerializer{}}
	for _, value := range []any{
		map[string]any{"nil": []string(nil), "empty": []string{}, "html": "<>&日本語\u2028", "time": time.Unix(0, 0).UTC()},
		struct {
			Name  string `json:"name"`
			Count int    `json:"count,omitempty"`
		}{Name: "test"},
		json.RawMessage(`{"nested":[1,true,null]}`),
	} {
		for _, indent := range []string{"", "  "} {
			var outputs []string
			for _, serializer := range serializers {
				recorder := httptest.NewRecorder()
				context := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), recorder)
				if err := serializer.Serialize(context, value, indent); err != nil {
					t.Fatal(err)
				}
				outputs = append(outputs, recorder.Body.String())
			}
			if outputs[0] != outputs[1] {
				t.Fatalf("encoding mismatch: %q / %q", outputs[0], outputs[1])
			}
		}
	}
	for _, input := range []string{`{"name":"日本語"}`, `{"NAME":"upper"}`, `{"name":"a","name":"b"}`, `{"name":null}`, `{"name":1}`, `{"name":`, `{} {}`, "", `{"name":"\ud800"}`} {
		var values []any
		var rejected []bool
		for _, serializer := range serializers {
			var value struct {
				Name string `json:"name"`
			}
			context := echo.New().NewContext(httptest.NewRequest("POST", "/", strings.NewReader(input)), httptest.NewRecorder())
			err := serializer.Deserialize(context, &value)
			rejected = append(rejected, err != nil)
			values = append(values, value)
		}
		if rejected[0] != rejected[1] || !reflect.DeepEqual(values[0], values[1]) {
			t.Fatalf("decoding mismatch for %q: %v %v", input, values, rejected)
		}
	}
}
