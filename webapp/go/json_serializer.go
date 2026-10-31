package isureal

import (
	"bytes"
	"sync"

	json "github.com/goccy/go-json"
	"github.com/labstack/echo/v5"
)

type fastJSONSerializer struct{}

var requestJSONBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (fastJSONSerializer) Serialize(c *echo.Context, value any, indent string) error {
	encoder := json.NewEncoder(c.Response())
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	return encoder.Encode(value)
}

func (fastJSONSerializer) Deserialize(c *echo.Context, value any) error {
	// Match Echo's full-body Unmarshal semantics, including trailing-data rejection.
	buffer := requestJSONBuffers.Get().(*bytes.Buffer)
	buffer.Reset()
	defer func() {
		if buffer.Cap() <= 64<<10 {
			requestJSONBuffers.Put(buffer)
		}
	}()
	if _, err := buffer.ReadFrom(c.Request().Body); err != nil {
		return echo.ErrBadRequest.Wrap(err)
	}
	if err := json.Unmarshal(buffer.Bytes(), value); err != nil {
		return echo.ErrBadRequest.Wrap(err)
	}
	return nil
}
