package protocol

import (
	"bytes"
	"encoding/json"
)

// LocationLink is the richer definition-result shape a server returns when the
// client advertises link support: it points at a target document and ranges
// rather than a bare Location.
type LocationLink struct {
	OriginSelectionRange *Range `json:"originSelectionRange,omitempty"`
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// asLocation collapses a LocationLink to a Location, preferring the precise
// selection range over the full target range.
func (l LocationLink) asLocation() Location {
	rng := l.TargetSelectionRange
	if rng == (Range{}) {
		rng = l.TargetRange
	}
	return Location{URI: l.TargetURI, Range: rng}
}

// DecodeLocations normalises a textDocument/definition result to []Location.
// Per the LSP spec the result is Location | Location[] | LocationLink[] | null,
// and some servers also return a bare single Location object (e.g. zls), which
// breaks a plain decode into []Location. Returns nil for null/empty.
func DecodeLocations(raw json.RawMessage) ([]Location, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	if trimmed[0] == '[' {
		return decodeLocationArray(trimmed)
	}
	return decodeSingleLocation(trimmed)
}

// decodeLocationArray decodes a JSON array as []Location, falling back to
// []LocationLink when the entries carry targetUri rather than uri.
func decodeLocationArray(raw json.RawMessage) ([]Location, error) {
	var locs []Location
	if err := json.Unmarshal(raw, &locs); err == nil && locationsResolved(locs) {
		return locs, nil
	}
	var links []LocationLink
	if err := json.Unmarshal(raw, &links); err != nil {
		return nil, err
	}
	return linksToLocations(links), nil
}

// decodeSingleLocation decodes a single JSON object as a Location, falling back
// to a LocationLink.
func decodeSingleLocation(raw json.RawMessage) ([]Location, error) {
	var loc Location
	if err := json.Unmarshal(raw, &loc); err == nil && loc.URI != "" {
		return []Location{loc}, nil
	}
	var link LocationLink
	if err := json.Unmarshal(raw, &link); err != nil {
		return nil, err
	}
	if link.TargetURI == "" {
		return nil, nil
	}
	return []Location{link.asLocation()}, nil
}

// locationsResolved reports whether every decoded Location carries a URI. A
// LocationLink[] decoded into []Location yields empty URIs (the field is
// targetUri, not uri), so an empty URI signals the wrong shape was used.
func locationsResolved(locs []Location) bool {
	for _, l := range locs {
		if l.URI == "" {
			return false
		}
	}
	return len(locs) > 0
}

func linksToLocations(links []LocationLink) []Location {
	out := make([]Location, 0, len(links))
	for _, l := range links {
		out = append(out, l.asLocation())
	}
	return out
}
