package scrape

import "strings"

// Attributes are this catalogue's own filterable fields, guessed from whatever
// the source called them.
//
// Guessed, and labelled as such: the admin panel shows these pre-filled and a
// human confirms them. Writing them straight into the catalogue would put
// "Rose Gold-Toned" in a colour filter that everything else spells "Gold", and
// the filters are only as useful as they are consistent.
type Attributes struct {
	Gender        string `json:"gender,omitempty"`
	DialColor     string `json:"dialColor,omitempty"`
	DialShape     string `json:"dialShape,omitempty"`
	DialType      string `json:"dialType,omitempty"`
	DialThickness string `json:"dialThickness,omitempty"`
	StrapColor    string `json:"strapColor,omitempty"`
	StrapMaterial string `json:"strapMaterial,omitempty"`
	Style         string `json:"style,omitempty"`
	// MainCategory is "Men" or "Women" -- the only two this catalogue has.
	MainCategory string `json:"mainCategory,omitempty"`
}

// specAliases maps our field to the labels sources actually use, most
// specific first.
var specAliases = map[string][]string{
	"dialColor":     {"dial colour", "dial color", "dial", "case colour", "case color", "colour", "color"},
	"dialShape":     {"dial shape", "case shape", "shape", "screen shape"},
	"dialType":      {"display type", "dial type", "display", "screen type", "movement type", "watch movement", "movement", "type"},
	"dialThickness": {"case thickness", "dial thickness", "thickness", "case diameter", "dial diameter", "screen size", "diameter"},
	"strapColor":    {"strap colour", "strap color", "band colour", "band color", "bracelet colour", "bracelet color"},
	"strapMaterial": {"strap material", "band material type", "band material", "bracelet material", "strap type", "band type", "material type", "material"},
	"style":         {"watch style", "style", "occasion", "ideal for"},
}

// identifierLabels are labels that name a product code rather than describe
// it. "Style Name" and "Style Code" contain "style" but are model names, and
// filling the Style filter with "ColorFit Pulse 2 Max" is worse than leaving
// it blank.
var identifierLabels = []string{"style name", "style code", "model name", "model number", "reference"}

// modelAliases are the labels a source uses for the manufacturer's own model
// code, which is the most reliable way to tell two watches apart.
var modelAliases = []string{"item model number", "model number", "model name", "model", "style code", "reference number"}

// guessAttributes reads the draft's specs and title for the attributes this
// catalogue filters on.
func guessAttributes(d *Draft) *Attributes {
	if d == nil {
		return nil
	}

	lookup := make(map[string]string, len(d.Specs))
	for _, spec := range d.Specs {
		lookup[strings.ToLower(spec.Label)] = spec.Value
	}

	find := func(field string) string {
		for _, alias := range specAliases[field] {
			// Exact label first: "dial" must not win over "dial colour" just
			// because it appears earlier in the table.
			if value, ok := lookup[alias]; ok {
				return value
			}
		}
		for _, alias := range specAliases[field] {
			for label, value := range lookup {
				if strings.Contains(label, alias) && !isIdentifierLabel(label) {
					return value
				}
			}
		}
		return ""
	}

	attrs := &Attributes{
		DialColor:     collapseSpace(find("dialColor")),
		DialShape:     collapseSpace(find("dialShape")),
		DialType:      collapseSpace(find("dialType")),
		DialThickness: collapseSpace(find("dialThickness")),
		StrapColor:    collapseSpace(find("strapColor")),
		StrapMaterial: collapseSpace(find("strapMaterial")),
		Style:         collapseSpace(find("style")),
	}

	if d.ModelNo == "" {
		for _, alias := range modelAliases {
			if value := collapseSpace(lookup[alias]); value != "" && len(value) < 60 {
				d.ModelNo = value
				break
			}
		}
	}

	attrs.Gender = guessGender(d, lookup)
	switch attrs.Gender {
	case "Men":
		attrs.MainCategory = "Men"
	case "Women":
		attrs.MainCategory = "Women"
	}

	// A value long enough to be a sentence is a description, not an
	// attribute, and would poison a filter list.
	for _, field := range []*string{
		&attrs.DialColor, &attrs.DialShape, &attrs.DialType, &attrs.DialThickness,
		&attrs.StrapColor, &attrs.StrapMaterial, &attrs.Style,
	} {
		if len(*field) > 40 {
			*field = ""
		}
	}

	return attrs
}

func isIdentifierLabel(label string) bool {
	for _, id := range identifierLabels {
		if strings.Contains(label, id) {
			return true
		}
	}
	return false
}

// guessGender reads who the watch is sold to, from the spec table first and
// the product name second.
func guessGender(d *Draft, lookup map[string]string) string {
	candidates := []string{
		lookup["target gender"], lookup["gender"], lookup["department"],
		lookup["ideal for"], lookup["suitable for"], d.Name, d.Category,
	}
	for _, candidate := range candidates {
		switch {
		case candidate == "":
			continue
		case containsWord(candidate, "unisex"):
			return "Unisex"
		case containsAnyWord(candidate, "women", "woman", "womens", "women's", "ladies", "lady", "girls", "girl", "her"):
			return "Women"
		case containsAnyWord(candidate, "men", "mens", "men's", "man", "boys", "boy", "gents", "him"):
			return "Men"
		}
	}
	return ""
}

func containsWord(haystack, word string) bool {
	return containsAnyWord(haystack, word)
}

// containsAnyWord matches whole words case-insensitively, so "Women" does not
// match inside "Womenswear" only by accident of substring, and -- the case
// that matters -- "men" does not match inside "women".
func containsAnyWord(haystack string, words ...string) bool {
	fields := strings.FieldsFunc(strings.ToLower(haystack), func(r rune) bool {
		return !('a' <= r && r <= 'z') && !('0' <= r && r <= '9') && r != '\''
	})
	set := make(map[string]bool, len(fields))
	for _, field := range fields {
		set[field] = true
	}
	for _, word := range words {
		if set[strings.ToLower(word)] {
			return true
		}
	}
	return false
}
