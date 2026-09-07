package models

import "testing"

func sampleCategories() []Category {
	return []Category{
		{Name: "Men", Subcategories: []Subcategory{
			{Name: "Gold watch", ImageURL: "gold.jpg"},
			{Name: "Leather watch"},
		}},
		{Name: "Women", Subcategories: []Subcategory{
			{Name: "Rose Gold"},
			{Name: "Leather watch"},
		}},
	}
}

// TestAutoTilesFollowLiveTree pins the default behaviour: every subcategory
// becomes a tile, in tree order, with no configuration required.
func TestAutoTilesFollowLiveTree(t *testing.T) {
	content := CategoryTilesContent{Enabled: true, AutoFromCategories: true}

	tiles, warnings := ResolveCategoryTiles(content, sampleCategories())

	if len(tiles) != 4 {
		t.Fatalf("got %d tiles, want 4", len(tiles))
	}
	if len(warnings) != 0 {
		t.Errorf("auto mode produced warnings: %v", warnings)
	}
	if tiles[0].Label != "Gold watch" || tiles[0].MainCategory != "Men" {
		t.Errorf("first tile = %+v, want Men/Gold watch", tiles[0])
	}
	if tiles[0].Image != "gold.jpg" {
		t.Errorf("tile did not inherit the live category image: %q", tiles[0].Image)
	}
}

// TestCuratedTilesRespectOrderAndEnabled covers the merchandiser's controls.
func TestCuratedTilesRespectOrderAndEnabled(t *testing.T) {
	content := CategoryTilesContent{
		Enabled: true,
		Tiles: []CategoryTile{
			{ID: "b", Enabled: true, Order: 2, Source: TileSourceSubcategory, Value: "Men > Gold watch"},
			{ID: "a", Enabled: true, Order: 1, Source: TileSourceSubcategory, Value: "Women > Rose Gold"},
			{ID: "off", Enabled: false, Order: 0, Source: TileSourceSubcategory, Value: "Men > Leather watch"},
		},
	}

	tiles, warnings := ResolveCategoryTiles(content, sampleCategories())

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(tiles) != 2 {
		t.Fatalf("got %d tiles, want 2 (the disabled one must be omitted)", len(tiles))
	}
	if tiles[0].ID != "a" || tiles[1].ID != "b" {
		t.Errorf("tiles are not in the admin's order: %s, %s", tiles[0].ID, tiles[1].ID)
	}
}

// TestBrokenReferenceIsOmittedNotSubstituted is the safety guarantee: a tile
// pointing at a category that no longer exists must disappear, must not crash,
// and must NEVER be quietly replaced by a different category.
func TestBrokenReferenceIsOmittedNotSubstituted(t *testing.T) {
	content := CategoryTilesContent{
		Enabled: true,
		Tiles: []CategoryTile{
			{ID: "ok", Enabled: true, Order: 1, Source: TileSourceSubcategory, Value: "Men > Gold watch"},
			{ID: "gone", Enabled: true, Order: 2, Source: TileSourceSubcategory, Value: "Men > Titanium watch"},
			{ID: "gone-cat", Enabled: true, Order: 3, Source: TileSourceCategory, Value: "Children"},
		},
	}

	tiles, warnings := ResolveCategoryTiles(content, sampleCategories())

	if len(tiles) != 1 {
		t.Fatalf("got %d tiles, want 1 -- broken references must be omitted", len(tiles))
	}
	if tiles[0].ID != "ok" {
		t.Errorf("wrong tile survived: %s", tiles[0].ID)
	}
	if len(warnings) != 2 {
		t.Fatalf("got %d warnings, want 2", len(warnings))
	}
	for _, w := range warnings {
		if w.TileID != "gone" && w.TileID != "gone-cat" {
			t.Errorf("unexpected warning for %q", w.TileID)
		}
		if w.Message == "" {
			t.Errorf("warning for %q has no message", w.TileID)
		}
	}
}

// TestTileOverridesWinOverLiveValues confirms the admin can relabel and
// re-image a tile without touching the category itself.
func TestTileOverridesWinOverLiveValues(t *testing.T) {
	content := CategoryTilesContent{
		Enabled: true,
		Tiles: []CategoryTile{{
			ID: "hero", Enabled: true, Source: TileSourceSubcategory,
			Value: "Men > Gold watch",
			Label: "The Gold Edit", Image: "custom.jpg", Href: "/custom",
			Subtitle: "Hand-picked",
		}},
	}

	tiles, _ := ResolveCategoryTiles(content, sampleCategories())

	if len(tiles) != 1 {
		t.Fatalf("got %d tiles, want 1", len(tiles))
	}
	got := tiles[0]
	if got.Label != "The Gold Edit" || got.Image != "custom.jpg" ||
		got.Href != "/custom" || got.Subtitle != "Hand-picked" {
		t.Errorf("overrides were not applied: %+v", got)
	}
	// The resolved reference is still carried, so counts can be scoped.
	if got.MainCategory != "Men" || got.Subcategory != "Gold watch" {
		t.Errorf("resolved reference lost: %+v", got)
	}
}

// TestDisabledSectionRendersNothing.
func TestDisabledSectionRendersNothing(t *testing.T) {
	content := CategoryTilesContent{Enabled: false, AutoFromCategories: true}

	tiles, warnings := ResolveCategoryTiles(content, sampleCategories())

	if len(tiles) != 0 || len(warnings) != 0 {
		t.Errorf("disabled section produced output: %d tiles, %d warnings", len(tiles), len(warnings))
	}
}

// TestPublicNavigationFiltersAndOrders covers what the storefront is allowed to
// receive: enabled entries with a destination, in the admin's order.
func TestPublicNavigationFiltersAndOrders(t *testing.T) {
	nav := NavigationContent{
		Primary: []NavItem{
			{ID: "b", Label: "B", Href: "/b", Enabled: true, Order: 2},
			{ID: "a", Label: "A", Href: "/a", Enabled: true, Order: 1},
			{ID: "off", Label: "Off", Href: "/off", Enabled: false, Order: 0},
			{ID: "nohref", Label: "No href", Enabled: true, Order: 0},
		},
		Footer: []FooterColumn{
			{ID: "keep", Heading: "Keep", Enabled: true, Order: 1, Items: []NavItem{
				{ID: "x", Label: "X", Href: "/x", Enabled: true, Order: 1},
			}},
			{ID: "empty", Heading: "Empty", Enabled: true, Order: 2, Items: []NavItem{
				{ID: "y", Label: "Y", Href: "/y", Enabled: false, Order: 1},
			}},
			{ID: "off", Heading: "Off", Enabled: false, Order: 3},
		},
	}

	got := nav.PublicNavigation()

	if len(got.Primary) != 2 {
		t.Fatalf("got %d primary items, want 2", len(got.Primary))
	}
	if got.Primary[0].ID != "a" || got.Primary[1].ID != "b" {
		t.Errorf("primary not ordered: %s, %s", got.Primary[0].ID, got.Primary[1].ID)
	}
	if len(got.Footer) != 1 || got.Footer[0].ID != "keep" {
		t.Errorf("footer columns wrong: %+v", got.Footer)
	}
}

// TestWithDefaultsFillsLegacyDocument is the backward-compatibility guarantee:
// a document stored before these sections existed must still render a complete
// storefront rather than losing its menus.
func TestWithDefaultsFillsLegacyDocument(t *testing.T) {
	// What an older document decodes to: the sections it knew about, and zero
	// values for the ones added later.
	legacy := StorefrontContent{
		Hero: HeroContent{Enabled: true, Eyebrow: "Kept"},
	}

	filled := legacy.WithDefaults()

	if len(filled.Navigation.Primary) == 0 {
		t.Error("navigation was not filled from defaults")
	}
	if len(filled.Navigation.Footer) == 0 {
		t.Error("footer columns were not filled from defaults")
	}
	if !filled.CategoryTiles.Enabled || !filled.CategoryTiles.AutoFromCategories {
		t.Error("category tiles were not filled from defaults")
	}
	if len(filled.Rails) == 0 {
		t.Error("rails were not filled from defaults")
	}
	// The stored section must survive untouched.
	if filled.Hero.Eyebrow != "Kept" {
		t.Errorf("stored hero was overwritten: %q", filled.Hero.Eyebrow)
	}
}
