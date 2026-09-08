package browser

import (
	"encoding/json"
	"io"
)

func jsonDecodeReader(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
