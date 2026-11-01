package hubclient

import "encoding/json"

func decode(b []byte, v any) error { return json.Unmarshal(b, v) }
