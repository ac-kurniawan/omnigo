package codexprofile

type Profile struct {
	Levels  []string
	Default string
}

var builtin = map[string]Profile{
	"gpt-6-astra":   {Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "low"},
	"gpt-6.1-sol":   {Levels: []string{"low", "medium", "high", "xhigh", "max"}, Default: "medium"},
	"gpt-5.6-sol":   {Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "low"},
	"gpt-6-sol":     {Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "medium"},
	"gpt-5.6-terra": {Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "medium"},
	"gpt-6-luna":    {Levels: []string{"low", "medium", "high", "xhigh", "max"}, Default: "medium"},
	"gpt-5.6-luna":  {Levels: []string{"low", "medium", "high", "xhigh", "max"}, Default: "medium"},
	"gpt-5.5":       {Levels: []string{"low", "medium", "high", "xhigh"}, Default: "medium"},
}

func Known(model string) (Profile, bool) {
	profile, ok := builtin[model]
	return profile, ok
}
