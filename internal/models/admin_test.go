package models

import "testing"

// Suspended and banned sellers must be hidden from the public; everyone else
// keeps a public store.
func TestStatusStoreVisible(t *testing.T) {
	cases := map[Status]bool{
		StatusActive:     true,
		StatusRestricted: true,
		StatusSuspended:  false,
		StatusBanned:     false,
	}
	for st, want := range cases {
		if got := st.StoreVisible(); got != want {
			t.Errorf("Status(%q).StoreVisible() = %v, want %v", st, got, want)
		}
	}
}
