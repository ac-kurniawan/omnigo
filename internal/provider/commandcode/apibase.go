package commandcode

// APIBaseFor returns the API base URL for a configured base URL, stripping
// the /alpha/generate suffix when present.
func APIBaseFor(baseURL string) string {
	return deriveAPIBase(baseURL)
}
