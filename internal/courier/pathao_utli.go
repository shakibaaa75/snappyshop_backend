package courier

import (
	"net/url"
	"sort"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
func sortStrings(s []string)         { sort.Strings(s) }
