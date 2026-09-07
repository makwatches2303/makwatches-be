package models

import (
	"fmt"
	"sort"
	"strings"
)

// ResolvedCategoryTile is a tile that has been matched against the live
// category tree and is safe to render.
//
// It is a projection, not a stored record: the storefront document holds only
// a reference, and this is what that reference currently resolves to.
type ResolvedCategoryTile struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Subtitle string `json:"subtitle,omitempty"`
	Href     string `json:"href"`
	Image    string `json:"image,omitempty"`

	// The resolved category path, for the storefront to scope counts by.
	MainCategory string `json:"mainCategory,omitempty"`
	Subcategory  string `json:"subcategory,omitempty"`
}

// TileWarning reports a tile that could not be resolved.
//
// Surfaced on the admin read so a broken reference is visible and fixable,
// rather than silently vanishing from the storefront with no explanation.
type TileWarning struct {
	TileID  string `json:"tileId"`
	Source  string `json:"source"`
	Value   string `json:"value"`
	Message string `json:"message"`
}

// categorySlug mirrors the frontend's slugify so /category/<slug> round-trips.
func categorySlug(value string) string {
	var b strings.Builder
	lastHyphen := true

	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}

// splitTileValue splits "Men > Gold watch" into its parts. A value with no
// separator is a top-level category.
func splitTileValue(value string) (parent string, child string) {
	parts := strings.SplitN(value, CategoryTileSeparator, 2)
	parent = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		child = strings.TrimSpace(parts[1])
	}
	return parent, child
}

// equalFold compares category names the way the catalogue actually stores them:
// casing and surrounding whitespace vary between records.
func equalFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// ResolveCategoryTiles turns the stored configuration into renderable tiles.
//
// Two modes:
//
//   - AutoFromCategories: one tile per subcategory, in tree order. This is the
//     default and keeps the section correct as the catalogue grows.
//   - Curated: the admin's own Tiles, in their order, each resolved against the
//     live tree.
//
// A curated tile whose category no longer exists is OMITTED and reported as a
// warning. It is never replaced by a different category: silently substituting
// one would show the shopper a tile the merchandiser did not choose, which is
// worse than showing one fewer tile.
func ResolveCategoryTiles(
	content CategoryTilesContent,
	categories []Category,
) ([]ResolvedCategoryTile, []TileWarning) {
	if !content.Enabled {
		return nil, nil
	}

	if content.AutoFromCategories || len(content.Tiles) == 0 {
		return autoTiles(categories), nil
	}

	tiles := append([]CategoryTile(nil), content.Tiles...)
	sort.SliceStable(tiles, func(i, j int) bool { return tiles[i].Order < tiles[j].Order })

	resolved := make([]ResolvedCategoryTile, 0, len(tiles))
	warnings := make([]TileWarning, 0)

	for _, tile := range tiles {
		if !tile.Enabled {
			continue
		}

		out, err := resolveTile(tile, categories)
		if err != nil {
			warnings = append(warnings, TileWarning{
				TileID:  tile.ID,
				Source:  tile.Source,
				Value:   tile.Value,
				Message: err.Error(),
			})
			continue
		}
		resolved = append(resolved, out)
	}

	return resolved, warnings
}

// autoTiles derives one tile per subcategory from the live tree.
func autoTiles(categories []Category) []ResolvedCategoryTile {
	out := make([]ResolvedCategoryTile, 0, 8)

	for _, category := range categories {
		for _, sub := range category.Subcategories {
			name := strings.TrimSpace(sub.Name)
			if name == "" {
				continue
			}
			out = append(out, ResolvedCategoryTile{
				ID:           categorySlug(category.Name + "-" + name),
				Label:        name,
				Href:         categoryHref(category.Name, name),
				Image:        sub.ImageURL,
				MainCategory: category.Name,
				Subcategory:  name,
			})
		}
	}

	return out
}

// resolveTile matches one curated reference against the live tree.
func resolveTile(tile CategoryTile, categories []Category) (ResolvedCategoryTile, error) {
	value := strings.TrimSpace(tile.Value)
	if value == "" {
		return ResolvedCategoryTile{}, fmt.Errorf("tile has no category reference")
	}

	parent, child := splitTileValue(value)

	switch tile.Source {
	case TileSourceSubcategory:
		for _, category := range categories {
			if child != "" && !equalFold(category.Name, parent) {
				continue
			}
			for _, sub := range category.Subcategories {
				// A bare value ("Gold watch") matches the first tree entry with
				// that name; a qualified one ("Men > Gold watch") must match
				// both halves.
				target := child
				if target == "" {
					target = parent
				}
				if !equalFold(sub.Name, target) {
					continue
				}
				return buildTile(tile, category.Name, strings.TrimSpace(sub.Name), sub.ImageURL), nil
			}
		}
		return ResolvedCategoryTile{}, fmt.Errorf("subcategory %q no longer exists", value)

	case TileSourceCategory, "":
		for _, category := range categories {
			if equalFold(category.Name, parent) {
				return buildTile(tile, category.Name, "", ""), nil
			}
		}
		return ResolvedCategoryTile{}, fmt.Errorf("category %q no longer exists", value)

	default:
		return ResolvedCategoryTile{}, fmt.Errorf("unknown tile source %q", tile.Source)
	}
}

// buildTile applies the admin's overrides over the live category's own values.
func buildTile(tile CategoryTile, mainCategory, subcategory, image string) ResolvedCategoryTile {
	label := tile.Label
	if label == "" {
		if subcategory != "" {
			label = subcategory
		} else {
			label = mainCategory
		}
	}

	href := tile.Href
	if href == "" {
		href = categoryHref(mainCategory, subcategory)
	}

	if tile.Image != "" {
		image = tile.Image
	}

	id := tile.ID
	if id == "" {
		id = categorySlug(mainCategory + "-" + subcategory)
	}

	return ResolvedCategoryTile{
		ID:           id,
		Label:        label,
		Subtitle:     tile.Subtitle,
		Href:         href,
		Image:        image,
		MainCategory: mainCategory,
		Subcategory:  subcategory,
	}
}

// categoryHref builds the storefront destination for a category reference.
func categoryHref(mainCategory, subcategory string) string {
	if subcategory == "" {
		// A top-level category has no dedicated route; the shop scoped to it is
		// the closest true destination.
		return "/shop?mainCategory=" + urlQueryEscape(mainCategory)
	}
	return "/category/" + categorySlug(subcategory) +
		"?mainCategory=" + urlQueryEscape(mainCategory)
}

// urlQueryEscape percent-encodes a query value. Kept local rather than pulling
// net/url into the model layer for one call.
func urlQueryEscape(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == '~':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('+')
		default:
			for _, c := range []byte(string(r)) {
				b.WriteString(fmt.Sprintf("%%%02X", c))
			}
		}
	}
	return b.String()
}
