package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Keys that dgdtools had before 0.4 and has no more. A file that still
// has one is told what took its place, instead of only that the key is
// unknown.

// removedTop are the top-level keys that are gone, and what to say.
var removedTop = map[string]string{
	"extends":       "extends removed: put per-directory settings in a nested dgdtools.yml with root: false, and share a mudlib's setup with a profile",
	"include_dirs":  driverKey("include_dirs"),
	"include_file":  driverKey("include_file"),
	"auto_objects":  driverKey("auto_objects"),
	"driver_object": driverKey("driver_object"),
	"create":        "create moved: use conventions.create (default create)",
}

// driverKey says where a driver setting that was a top-level key, or a
// key of dialect:, is said now.
func driverKey(old string) string {
	key, as := old, ""
	switch old {
	case "auto_objects":
		key, as = "auto_object", " (one object, as in the driver config)"
	case "dialect.typechecking":
		key = "typechecking"
	}
	return fmt.Sprintf("%s moved: set driver.%s%s, or let dgdtools read it from driver_config, the DGD driver's config file (a partial checkout gets it from its profile)", old, key, as)
}

// checkRemoved reports the first key of an older dgdtools that the file
// data has, saying what replaces it.
func checkRemoved(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return nil // the decoder that follows says what is wrong
	}
	top := doc.Content[0]
	fail := func(k *yaml.Node, msg string) error {
		return fmt.Errorf("line %d: %s", k.Line, msg)
	}
	for k, v := range pairs(top) {
		if msg, ok := removedTop[k.Value]; ok {
			return fail(k, msg)
		}
		switch k.Value {
		case "profile_build":
			for k2 := range pairs(v) {
				if k2.Value == "driver_config" {
					return fail(k2, "profile_build.driver_config moved: use top-level driver_config")
				}
			}
		case "dialect":
			for k2 := range pairs(v) {
				if k2.Value == "typechecking" {
					return fail(k2, driverKey("dialect.typechecking"))
				}
			}
		case "conventions":
			for k2, v2 := range pairs(v) {
				switch k2.Value {
				case "specifier_macros":
					return fail(k2, "conventions.specifier_macros removed: use conventions.macros: { "+specifiers(v2)+" }")
				case "format_registry":
					return fail(k2, "conventions.format_registry removed: sscanf is the scanf-style function dgdtools checks")
				}
			}
		case "lint":
			for k2, v2 := range pairs(v) {
				switch k2.Value {
				case "fail_on":
					return fail(k2, "lint.fail_on removed: pass --fail-on to dgdtools lint (default error)")
				case "rules":
					if err := maxPerFile("lint.rules", v2); err != nil {
						return err
					}
				case "overrides":
					if v2.Kind != yaml.SequenceNode {
						continue
					}
					for i, o := range v2.Content {
						for k3, v3 := range pairs(o) {
							if k3.Value == "rules" {
								if err := maxPerFile(fmt.Sprintf("lint.overrides[%d].rules", i), v3); err != nil {
									return err
								}
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// maxPerFile reports a max_per_file in a rules mapping.
func maxPerFile(where string, rules *yaml.Node) error {
	for k, v := range pairs(rules) {
		for k2 := range pairs(v) {
			if k2.Value == "max_per_file" {
				return fmt.Errorf("line %d: %s.%s.max_per_file removed: pass --max-per-file to dgdtools lint", k2.Line, where, k.Value)
			}
		}
	}
	return nil
}

// specifiers renders the names of a specifier_macros list as entries of
// conventions.macros.
func specifiers(list *yaml.Node) string {
	var names []string
	switch list.Kind {
	case yaml.SequenceNode:
		for _, n := range list.Content {
			if n.Kind == yaml.ScalarNode {
				names = append(names, n.Value+": specifier")
			}
		}
	case yaml.ScalarNode:
		if list.Value != "" {
			names = append(names, list.Value+": specifier")
		}
	}
	if len(names) == 0 {
		names = []string{"public: specifier"}
	}
	return strings.Join(names, ", ")
}

// pairs yields the keys and values of a mapping node; nothing for any
// other node.
func pairs(n *yaml.Node) func(func(k, v *yaml.Node) bool) {
	return func(yield func(k, v *yaml.Node) bool) {
		if n == nil || n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if !yield(n.Content[i], n.Content[i+1]) {
				return
			}
		}
	}
}
