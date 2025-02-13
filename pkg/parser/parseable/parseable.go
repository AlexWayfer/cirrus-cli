package parseable

import (
	"github.com/cirruslabs/cirrus-cli/pkg/parser/nameable"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/node"
	"github.com/lestrrat-go/jsschema"
	"regexp"
)

type Parseable interface {
	Parse(node *node.Node) error
	Schema() *schema.Schema
	CollectibleFields() []CollectibleField
	Proto() interface{}
}

type nodeFunc func(node *node.Node) error

type Field struct {
	name     nameable.Nameable
	required bool
	onFound  nodeFunc
	schema   *schema.Schema
}

type CollectibleField struct {
	Name    string
	onFound nodeFunc
	Schema  *schema.Schema
}

func (parser *DefaultParser) Parse(node *node.Node) error {
	// Evaluate collectible fields
	for _, field := range parser.collectibleFields {
		if err := evaluateCollectible(node, field); err != nil {
			return err
		}
	}

	for _, child := range node.Children {
		// double check collectible fields
		for _, collectibleField := range parser.collectibleFields {
			if collectibleField.Name == child.Name {
				if err := evaluateCollectible(node, collectibleField); err != nil {
					return err
				}
				break
			}
		}
	}

	seenFields := map[string]struct{}{}

	for _, child := range node.Children {
		for _, field := range parser.fields {
			if field.name.Matches(child.Name) {
				seenFields[field.name.MapKey()] = struct{}{}

				if err := field.onFound(child); err != nil {
					return err
				}

				break
			}
		}
	}

	for _, field := range parser.fields {
		_, seen := seenFields[field.name.MapKey()]

		if field.required && !seen {
			return node.ParserError("required field %q was not set", field.name.String())
		}
	}

	return nil
}

func (parser *DefaultParser) Schema() *schema.Schema {
	schema := &schema.Schema{
		Properties:           make(map[string]*schema.Schema),
		PatternProperties:    make(map[*regexp.Regexp]*schema.Schema),
		AdditionalItems:      &schema.AdditionalItems{Schema: nil},
		AdditionalProperties: &schema.AdditionalProperties{Schema: nil},
	}

	for _, field := range parser.fields {
		switch nameable := field.name.(type) {
		case *nameable.SimpleNameable:
			schema.Properties[nameable.Name()] = field.schema
		case *nameable.RegexNameable:
			schema.PatternProperties[nameable.Regex()] = field.schema
		}

		if field.required && !parser.Collectible() {
			schema.Required = append(schema.Required, field.name.String())
		}
	}

	for _, collectibleField := range parser.collectibleFields {
		schema.Properties[collectibleField.Name] = collectibleField.Schema
	}

	return schema
}

func (parser *DefaultParser) CollectibleFields() []CollectibleField {
	return parser.collectibleFields
}

func evaluateCollectible(node *node.Node, field CollectibleField) error {
	children := node.DeepFindCollectible(field.Name)

	if children == nil {
		return nil
	}

	return field.onFound(children)
}
