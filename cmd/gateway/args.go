package main

import (
	"fmt"
	"strconv"
	"strings"
)

// option returns the value that follows name. Commands here use the `--flag
// value` form only, matching the documented CLI.
func option(args []string, name string) (string, bool) {
	for index := 0; index < len(args); index++ {
		if args[index] == name && index+1 < len(args) {
			return args[index+1], true
		}
	}
	return "", false
}

// options collects every value bound to a repeatable option such as --model.
func options(args []string, name string) []string {
	var values []string
	for index := 0; index < len(args); index++ {
		if args[index] == name && index+1 < len(args) {
			values = append(values, args[index+1])
		}
	}
	return values
}

// numberOption parses a positive integer option and reports whether it was set.
func numberOption(args []string, name string) (int, bool, error) {
	raw, ok := option(args, name)
	if !ok {
		return 0, false, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, false, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, true, nil
}

// positionals returns the non-flag arguments, skipping the value of any option
// listed in valueOptions so `models add foo --context-window 8192` yields [foo].
func positionals(args []string, valueOptions ...string) []string {
	skip := make(map[string]bool, len(valueOptions))
	for _, name := range valueOptions {
		skip[name] = true
	}

	var values []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if skip[arg] {
			index++
			continue
		}
		if !strings.HasPrefix(arg, "--") {
			values = append(values, arg)
		}
	}
	return values
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}
