package ocppj

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	t.Run("call", func(t *testing.T) {
		m, err := Decode([]byte(`[2,"19223201","BootNotification",{"chargePointVendor":"VendorX","chargePointModel":"SingleSocketCharger"}]`))
		if err != nil {
			t.Fatal(err)
		}
		if m.Type != TypeCall || m.ID != "19223201" || m.Action != "BootNotification" {
			t.Errorf("got %+v", m)
		}
		if !strings.Contains(string(m.Payload), "VendorX") {
			t.Errorf("payload = %s", m.Payload)
		}
	})

	t.Run("call result", func(t *testing.T) {
		m, err := Decode([]byte(`[3,"19223201",{"status":"Accepted","currentTime":"2013-02-01T20:53:32.486Z","interval":300}]`))
		if err != nil {
			t.Fatal(err)
		}
		if m.Type != TypeCallResult || m.ID != "19223201" {
			t.Errorf("got %+v", m)
		}
	})

	t.Run("call error", func(t *testing.T) {
		m, err := Decode([]byte(`[4,"162376037","NotSupported","SetDisplayMessageRequest not implemented",{}]`))
		if err != nil {
			t.Fatal(err)
		}
		if m.Type != TypeCallError || m.ErrorCode != "NotSupported" || m.ErrorDescription != "SetDisplayMessageRequest not implemented" {
			t.Errorf("got %+v", m)
		}
	})
}

func TestDecodeRejectsMalformedFrames(t *testing.T) {
	cases := map[string]string{
		"not json":            `hello`,
		"not an array":        `{"a":1}`,
		"too short":           `[2,"1"]`,
		"unknown type":        `[9,"1",{}]`,
		"type not a number":   `["2","1","Heartbeat",{}]`,
		"id not a string":     `[2,1,"Heartbeat",{}]`,
		"call without action": `[2,"1",{}]`,
		"payload not object":  `[2,"1","Heartbeat",[]]`,
		"result too long":     `[3,"1",{},{}]`,
		"error too short":     `[4,"1","GenericError",""]`,
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(frame)); err == nil {
				t.Errorf("Decode(%s) succeeded, want an error", frame)
			}
		})
	}
}

func TestEncode(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{
			"call",
			Message{Type: TypeCall, ID: "7", Action: "Heartbeat"},
			`[2,"7","Heartbeat",{}]`,
		},
		{
			"call result",
			Message{Type: TypeCallResult, ID: "7", Payload: json.RawMessage(`{"currentTime":"2013-02-01T20:53:32Z"}`)},
			`[3,"7",{"currentTime":"2013-02-01T20:53:32Z"}]`,
		},
		{
			"call error",
			Message{Type: TypeCallError, ID: "7", ErrorCode: ErrNotImplemented, ErrorDescription: "no"},
			`[4,"7","NotImplemented","no",{}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.msg.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("Encode = %s, want %s", got, tc.want)
			}
		})
	}
}
