package configs

// ID is the unique ID given to each configuration. When a configuration
// changes, it gets a new ID.
type ID int

// Config is a configuration of a subsystem. It's a map of arbitrary field
// names to arbitrary values.
type Config map[string]interface{}

// ConfigView is what users get when they get a config.
type ConfigView struct {
	ID     ID     `json:"id"`
	Config Config `json:"config"`
}
