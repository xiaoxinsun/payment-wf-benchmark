package mapping

import "encoding/xml"

func unmarshalForTest(raw []byte, v any) error { return xml.Unmarshal(raw, v) }
