package accumulo

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// splitMergeability mirrors Accumulo 4's
// org.apache.accumulo.core.client.admin.TabletMergeability: a tablet is
// either never automatically mergeable, or becomes mergeable after a delay
// (a zero delay meaning "always, now").
type splitMergeability struct {
	never bool
	delay time.Duration
}

// neverMergeable is the mergeability Accumulo's TableOperations.addSplits
// applies to every user-requested split point
// (TabletMergeabilityUtil.userDefaultSplits uses TabletMergeability.never()).
func neverMergeable() splitMergeability {
	return splitMergeability{never: true}
}

// mergeableAfter is Accumulo's TabletMergeability.after(delay); a zero delay
// is TabletMergeability.always(). Shoal never sends these for AddTableSplits
// — they exist so the payload encoder can be validated against the full
// upstream encoding.
func mergeableAfter(delay time.Duration) splitMergeability {
	return splitMergeability{delay: delay}
}

// splitMergeabilityPayload is the exact JSON document Accumulo 4 places in
// each TABLE_SPLIT FATE argument after the three extent arguments. It is
// TabletMergeabilityUtil.GSonData, serialized by
// ByteArrayToBase64TypeAdapter.createBase64Gson():
//
//   - field order is the record component order: split, never, delay;
//   - split is the raw split row bytes in URL-safe, padded Base64
//     (java.util.Base64.getUrlEncoder(), i.e. Go's base64.URLEncoding);
//   - delay is nanoseconds and is omitted entirely when never is true,
//     because Gson suppresses null fields by default;
//   - the document has no whitespace and no HTML escaping
//     (GsonBuilder.disableHtmlEscaping()); the Base64 URL alphabet cannot
//     produce a character Go's encoding/json would escape.
type splitMergeabilityPayload struct {
	Split string `json:"split"`
	Never bool   `json:"never"`
	Delay *int64 `json:"delay,omitempty"`
}

// encodeSplitMergeability produces one TABLE_SPLIT FATE split argument.
// The result is byte-for-byte identical to
// TabletMergeabilityUtil.encodeAsBuffer for the same row and mergeability.
func encodeSplitMergeability(row []byte, mergeability splitMergeability) ([]byte, error) {
	payload := splitMergeabilityPayload{
		Split: base64.URLEncoding.EncodeToString(row),
		Never: mergeability.never,
	}
	if !mergeability.never {
		delay := mergeability.delay.Nanoseconds()
		payload.Delay = &delay
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("accumulo: encode split payload: %w", err)
	}
	return encoded, nil
}
