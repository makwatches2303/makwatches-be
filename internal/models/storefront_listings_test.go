package models

import "testing"

// The listing-page headers an admin edits in Storefront -> Page headings.
//
// These replaced literals in the storefront's page files, so the thing to
// prove is that moving them into content changed nothing: an untouched
// install still renders the wording it always did, a partially edited one
// keeps the shipped copy beside the admin's, and clearing a field cannot
// leave a page with no heading.

func TestDefaultListingHeadersMatchTheShippedCopy(t *testing.T) {
	d := DefaultStorefrontContent().Listings

	cases := map[string]struct {
		got             ListingHeader
		eyebrow, title  string
		wantDescription string
	}{
		"collection": {
			d.Collection, "The collection", "Every watch we make.",
			"The complete MAK catalogue. Filter by brand, price and availability.",
		},
		"men":   {d.Men, "For him", "The men's edit.", ""},
		"women": {d.Women, "For her", "The women's edit.", ""},
	}
	for name, tc := range cases {
		if tc.got.Eyebrow != tc.eyebrow {
			t.Errorf("%s eyebrow = %q, want %q", name, tc.got.Eyebrow, tc.eyebrow)
		}
		if tc.got.Title != tc.title {
			t.Errorf("%s title = %q, want %q", name, tc.got.Title, tc.title)
		}
		if tc.got.Description != tc.wantDescription {
			t.Errorf("%s description = %q, want %q", name, tc.got.Description, tc.wantDescription)
		}
	}
}

// A document saved before this section existed decodes with every header
// empty. It must still render the shipped copy, not three blank pages.
func TestStoredDocumentWithoutListingsGetsTheDefaults(t *testing.T) {
	stored := StorefrontContent{}
	filled := stored.WithDefaults()

	want := DefaultStorefrontContent().Listings
	if filled.Listings.Collection != want.Collection {
		t.Errorf("collection = %+v, want %+v", filled.Listings.Collection, want.Collection)
	}
	if filled.Listings.Men != want.Men {
		t.Errorf("men = %+v, want %+v", filled.Listings.Men, want.Men)
	}
	if filled.Listings.Women != want.Women {
		t.Errorf("women = %+v, want %+v", filled.Listings.Women, want.Women)
	}
}

// An admin's edit survives the defaults being merged over it -- otherwise
// saving a new headline would appear to work and then revert on the next read.
func TestAnEditedHeaderIsNotOverwrittenByDefaults(t *testing.T) {
	stored := StorefrontContent{
		Listings: ListingPagesContent{
			Collection: ListingHeader{Title: "Timeless style. Everyday confidence."},
		},
	}

	filled := stored.WithDefaults().Listings

	if filled.Collection.Title != "Timeless style. Everyday confidence." {
		t.Fatalf("title = %q, want the admin's own headline", filled.Collection.Title)
	}
	// ...and the fields they left alone still carry the shipped copy, rather
	// than the header going half-empty because one field was authored.
	d := DefaultStorefrontContent().Listings
	if filled.Collection.Eyebrow != d.Collection.Eyebrow {
		t.Errorf("eyebrow = %q, want the shipped %q", filled.Collection.Eyebrow, d.Collection.Eyebrow)
	}
	if filled.Collection.Description != d.Collection.Description {
		t.Errorf("description = %q, want the shipped default", filled.Collection.Description)
	}
	// The other pages are untouched by an edit to this one.
	if filled.Men != d.Men {
		t.Errorf("men = %+v, want the defaults", filled.Men)
	}
}

func TestListingHeaderIsEmpty(t *testing.T) {
	if !(ListingHeader{}).IsEmpty() {
		t.Error("a zero header does not report as empty")
	}
	for _, h := range []ListingHeader{
		{Eyebrow: "For him"},
		{Title: "The men's edit."},
		{Description: "Something."},
	} {
		if h.IsEmpty() {
			t.Errorf("%+v reports as empty", h)
		}
	}
}
