package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

type tool struct {
	name, namespace, kind, wire string
	declared                    bool
}
type toolSet struct {
	byKey        map[string]*tool
	byWire       map[string]*tool
	declarations []any
}

func toolKey(namespace, name, kind string) string { return namespace + "\x00" + name + "\x00" + kind }
func (s *toolSet) register(namespace, name, kind string) (*tool, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.ContainsRune(namespace, 0) {
		return nil, fmt.Errorf("tool name is missing or invalid")
	}
	key := toolKey(namespace, name, kind)
	if t := s.byKey[key]; t != nil {
		return t, nil
	}
	t := &tool{name: name, namespace: namespace, kind: kind, wire: fmt.Sprintf("tool_%d", len(s.byKey))}
	s.byKey[key] = t
	s.byWire[t.wire] = t
	return t, nil
}
func buildTools(declarations any, input []any) (*toolSet, error) {
	s := &toolSet{byKey: map[string]*tool{}, byWire: map[string]*tool{}}
	var add func(obj, string) error
	add = func(d obj, namespace string) error {
		kind := stringValue(d["type"])
		if kind == "namespace" {
			if namespace != "" {
				return fmt.Errorf("nested tool namespaces are unsupported")
			}
			name := stringValue(d["name"])
			if name == "" {
				return fmt.Errorf("tool namespace name is required")
			}
			members, ok := d["tools"].([]any)
			if !ok {
				return fmt.Errorf("namespace tools must be an array")
			}
			for _, member := range members {
				if err := add(object(member), name); err != nil {
					return err
				}
			}
			return nil
		}
		if kind != "function" && kind != "custom" {
			return fmt.Errorf("unsupported tool type %q", kind)
		}
		t, err := s.register(namespace, stringValue(d["name"]), kind)
		if err != nil {
			return err
		}
		for _, existing := range s.declarations {
			if object(object(existing)["function"])["name"] == t.wire {
				return fmt.Errorf("duplicate tool %q", t.name)
			}
		}
		qualified := t.name
		if namespace != "" {
			qualified = namespace + "." + t.name
		}
		description := "Tool: " + qualified + "\n" + stringValue(d["description"])
		parameters := d["parameters"]
		if kind == "custom" {
			format := object(d["format"])
			if d["format"] != nil && format == nil {
				return fmt.Errorf("custom tool format must be an object")
			}
			if format != nil && stringValue(format["type"]) != "text" {
				if stringValue(format["type"]) != "grammar" || stringValue(format["syntax"]) != "lark" || stringValue(format["definition"]) == "" {
					return fmt.Errorf("unsupported custom tool format")
				}
				// shortcut: Chat Completions cannot enforce Lark; the grammar remains model guidance.
				description += "\n\nFormat:\n```lark\n" + stringValue(format["definition"]) + "\n```"
			}
			parameters = obj{"type": "object", "properties": obj{"content": obj{"type": "string", "description": "The tool input following the specified format"}}, "required": []string{"content"}, "additionalProperties": false}
		} else if object(parameters) == nil {
			return fmt.Errorf("function tool parameters must be an object")
		}
		function := obj{"name": t.wire, "description": description, "parameters": parameters}
		if strict, ok := d["strict"].(bool); ok {
			function["strict"] = strict
		}
		t.declared = true
		s.declarations = append(s.declarations, obj{"type": "function", "function": function})
		return nil
	}
	if declarations != nil {
		list, ok := declarations.([]any)
		if !ok {
			return nil, fmt.Errorf("tools must be an array")
		}
		for _, d := range list {
			if err := add(object(d), ""); err != nil {
				return nil, err
			}
		}
	}
	// History may reference a tool no longer offered in the current declarations.
	for _, raw := range input {
		item := object(raw)
		kind := stringValue(item["type"])
		if kind == "function_call" || kind == "custom_tool_call" {
			k := "function"
			if kind == "custom_tool_call" {
				k = "custom"
			}
			if _, err := s.register(stringValue(item["namespace"]), stringValue(item["name"]), k); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}
func (s *toolSet) choice(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	if choice, ok := value.(string); ok {
		if choice == "auto" || choice == "none" || choice == "required" {
			return choice, nil
		}
		return nil, fmt.Errorf("unsupported tool_choice %q", choice)
	}
	d := object(value)
	kind := stringValue(d["type"])
	if kind != "function" && kind != "custom" {
		return nil, fmt.Errorf("unsupported tool_choice")
	}
	t := s.byKey[toolKey(stringValue(d["namespace"]), stringValue(d["name"]), kind)]
	if t == nil || !t.declared {
		return nil, fmt.Errorf("tool_choice references an undeclared tool")
	}
	return obj{"type": "function", "function": obj{"name": t.wire}}, nil
}
func (s *toolSet) output(name, args, callID, id string) (obj, error) {
	t := s.byWire[name]
	if t == nil || !t.declared {
		return nil, fmt.Errorf("upstream called unknown tool %q", name)
	}
	if callID == "" {
		return nil, fmt.Errorf("upstream tool call is missing its ID")
	}
	out := obj{"type": "function_call", "id": id, "call_id": callID, "name": t.name, "arguments": args, "status": "completed"}
	if t.namespace != "" {
		out["namespace"] = t.namespace
	}
	if t.kind == "custom" {
		var decoded obj
		if err := json.Unmarshal([]byte(args), &decoded); err != nil {
			return nil, fmt.Errorf("custom tool arguments are invalid JSON")
		}
		content, ok := decoded["content"].(string)
		if !ok || len(decoded) != 1 {
			return nil, fmt.Errorf("custom tool arguments require exactly one content string")
		}
		out["type"] = "custom_tool_call"
		out["input"] = content
		delete(out, "arguments")
	} else {
		var decoded obj
		if err := json.Unmarshal([]byte(args), &decoded); err != nil || decoded == nil {
			return nil, fmt.Errorf("upstream function arguments must be a JSON object")
		}
	}
	return out, nil
}
