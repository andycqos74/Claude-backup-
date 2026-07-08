package agent

import (
	"encoding/json"
	"errors"
	"os"
)

var errNotConnected = errors.New("not connected to server")

func hostnameOrEmpty() string {
	h, _ := os.Hostname()
	return h
}

func unmarshalMsg[T any](data json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(data, &v)
	return v, err
}
