package client

import (
	"fmt"
	"strings"
	"testing"
)

func TestJSONToMsgPackRejectsReservedTimeNames(t *testing.T) {
	for _, name := range []string{"time", "Time", "TIME", "tImE"} {
		for _, kind := range []string{"fields", "tags", "columns"} {
			if kind == "columns" && name == "time" {
				continue // the timestamp column itself is valid
			}
			t.Run(kind+"/"+name, func(t *testing.T) {
				var row string
				switch kind {
				case "fields":
					row = fmt.Sprintf(`{"m":"cpu","fields":{%q:1}}`, name)
				case "tags":
					row = fmt.Sprintf(`{"m":"cpu","fields":{"value":1},"tags":{%q:"label"}}`, name)
				case "columns":
					row = fmt.Sprintf(`{"m":"cpu","columns":{%q:[1700000000],"value":[1]}}`, name)
				}
				for _, doc := range []string{
					row,
					fmt.Sprintf(`[{"m":"cpu","fields":{"value":1}},%s]`, row),
					fmt.Sprintf(`{"batch":[{"m":"cpu","fields":{"value":1}},%s]}`, row),
				} {
					out, _, err := JSONToMsgPack(strings.NewReader(doc))
					if err == nil || !strings.Contains(err.Error(), "reserved") {
						t.Errorf("%s: error = %v; want a reserved timestamp name error", doc, err)
					}
					if len(out) != 0 {
						t.Error("invalid document produced MessagePack bytes")
					}
				}
			})
		}
	}
}

func TestJSONToMsgPackAcceptsTimestampAndOtherNames(t *testing.T) {
	for _, doc := range []string{
		`{"m":"cpu","t":1700000000,"fields":{"runtime":1,"time_value":2},"tags":{"timezone":"UTC"}}`,
		`{"m":"cpu","columns":{"time":[1700000000],"value":[1]}}`,
		`{"m":"cpu","fields":{"TİME":1,"ｔｉｍｅ":2},"tags":{"TİME":"a","ｔｉｍｅ":"b"}}`,
		`{"m":"cpu","columns":{"TİME":[1],"ｔｉｍｅ":[2]}}`,
	} {
		if _, _, err := JSONToMsgPack(strings.NewReader(doc)); err != nil {
			t.Errorf("valid names rejected: %v", err)
		}
	}
}
