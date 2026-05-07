package trace

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	BaggageHeader = "baggage"
	MetaTraceKey  = "meta-trace-id"
)

// InjectBaggageHTTP adds a meta-trace-id entry to the W3C Baggage header.
// If the header already has entries, the new entry is appended.
func InjectBaggageHTTP(req *http.Request, metaTraceID string) {
	entry := fmt.Sprintf("%s=%s", MetaTraceKey, metaTraceID)
	existing := req.Header.Get(BaggageHeader)
	if existing == "" {
		req.Header.Set(BaggageHeader, entry)
	} else {
		req.Header.Set(BaggageHeader, existing+","+entry)
	}
}

// InjectLabeledBaggage adds multiple key=value pairs to the W3C Baggage header,
// comma-separated. Existing entries are preserved.
func InjectLabeledBaggage(h http.Header, entries map[string]string) {
	var parts []string
	for k, v := range entries {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	if len(parts) == 0 {
		return
	}
	addition := strings.Join(parts, ",")
	existing := h.Get(BaggageHeader)
	if existing == "" {
		h.Set(BaggageHeader, addition)
	} else {
		h.Set(BaggageHeader, existing+","+addition)
	}
}

// InjectBaggageHeader adds a meta-trace-id entry to a raw http.Header map.
// Used for vegeta targets which expose Header directly.
func InjectBaggageHeader(h http.Header, metaTraceID string) {
	InjectLabeledBaggage(h, map[string]string{MetaTraceKey: metaTraceID})
}

// ExtractMetaTraceID parses the baggage header and returns the meta-trace-id value.
// Returns empty string if not found.
func ExtractMetaTraceID(header http.Header) string {
	baggage := header.Get(BaggageHeader)
	if baggage == "" {
		return ""
	}
	for _, item := range strings.Split(baggage, ",") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) == 2 && kv[0] == MetaTraceKey {
			return kv[1]
		}
	}
	return ""
}

// ValidateMetaTraceID ensures the ID is safe for W3C baggage transport.
// Baggage values must not contain comma, semicolon, equals, or whitespace.
func ValidateMetaTraceID(id string) error {
	if id == "" {
		return fmt.Errorf("trace: meta-trace-id must not be empty")
	}
	if strings.ContainsAny(id, ",;= \t") {
		return fmt.Errorf("trace: meta-trace-id contains baggage-unsafe characters: %q", id)
	}
	return nil
}
