package arborist

import (
	"errors"
	"testing"
)

func TestJSONResponse(t *testing.T) {
	t.Run("addErrorJSON", func(t *testing.T) {
		err := errors.New("example")
		response := JSONResponse{InternalError: err}
		response.Code = 500
		response.addErrorJSON()
		result := string(response.Bytes)
		expected := "{\"error\":\"example\",\"code\":500}"
		if result != expected {
			t.Logf("result: %s", result)
			t.Logf("expected: %s", expected)
			t.Fail()
		}

		err = errors.New("user error")
		response = JSONResponse{ExternalError: err}
		response.Code = 400
		response.addErrorJSON()
		result = string(response.Bytes)
		expected = "{\"error\":\"user error\",\"code\":400}"
		if result != expected {
			t.Logf("result: %s", result)
			t.Logf("expected: %s", expected)
			t.Fail()
		}
	})
}
