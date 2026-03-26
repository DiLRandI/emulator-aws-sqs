package model

import "encoding/json"

func decodeRegistry(raw []byte, into *Registry) error {
	return json.Unmarshal(raw, into)
}
