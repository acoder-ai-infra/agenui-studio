package documentcontract

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	LayoutSchema  = "layout_doc.v1"
	ElementSchema = "element_doc.v1"
	RuleSchema    = "atomic_rule_doc.v1"
)

type Strength string

const (
	StrengthRequired    Strength = "required"
	StrengthRecommended Strength = "recommended"
	StrengthOptional    Strength = "optional"
	StrengthForbidden   Strength = "forbidden"
)

type Document struct {
	DocumentSchema string       `json:"documentSchema"`
	ID             string       `json:"id"`
	Version        string       `json:"version"`
	Kind           string       `json:"kind"`
	Title          string       `json:"title"`
	Summary        string       `json:"summary"`
	AppliesTo      []string     `json:"appliesTo,omitempty"`
	NotFor         []string     `json:"notFor,omitempty"`
	Requires       []string     `json:"requires,omitempty"`
	References     []string     `json:"references,omitempty"`
	ConflictsWith  []string     `json:"conflictsWith,omitempty"`
	Layout         *LayoutBody  `json:"layout,omitempty"`
	Element        *ElementBody `json:"element,omitempty"`
	Rules          []AtomicRule `json:"rules,omitempty"`
}

type LayoutBody struct {
	ContentModes     []string     `json:"contentModes,omitempty"`
	Topology         []string     `json:"topology,omitempty"`
	Roles            []string     `json:"roles,omitempty"`
	Interactions     []string     `json:"interactions,omitempty"`
	DesignModes      []string     `json:"designModes,omitempty"`
	Nodes            []LayoutNode `json:"nodes"`
	Relations        []Relation   `json:"relations,omitempty"`
	Recipes          []Recipe     `json:"recipes,omitempty"`
	PositiveExamples []string     `json:"positiveExamples,omitempty"`
	NegativeExamples []string     `json:"negativeExamples,omitempty"`
}

type LayoutNode struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId,omitempty"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
	Capacity string `json:"capacity,omitempty"`
}

type Relation struct {
	Type        string   `json:"type"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	Strength    Strength `json:"strength"`
	Description string   `json:"description,omitempty"`
}

type Recipe struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	AppliesWhen  string   `json:"appliesWhen"`
	NotFor       string   `json:"notFor,omitempty"`
	Instructions []string `json:"instructions"`
}

type ElementBody struct {
	Purpose               string   `json:"purpose"`
	CatalogComponents     []string `json:"catalogComponents"`
	Rules                 []string `json:"rules,omitempty"`
	States                []string `json:"states,omitempty"`
	MutuallyExclusiveWith []string `json:"mutuallyExclusiveWith,omitempty"`
	Accessibility         []string `json:"accessibility,omitempty"`
}

type AtomicRule struct {
	ID          string   `json:"id"`
	Target      string   `json:"target"`
	Strength    Strength `json:"strength"`
	Effect      string   `json:"effect"`
	AppliesTo   []string `json:"appliesTo,omitempty"`
	NotFor      []string `json:"notFor,omitempty"`
	SourceTrace []string `json:"sourceTrace"`
}

func (d *Document) Normalize() {
	if d == nil {
		return
	}
	for _, values := range [][]string{d.AppliesTo, d.NotFor, d.Requires, d.References, d.ConflictsWith} {
		sort.Strings(values)
	}
	if d.Layout != nil {
		sort.Slice(d.Layout.Nodes, func(i, j int) bool { return d.Layout.Nodes[i].ID < d.Layout.Nodes[j].ID })
		sort.Slice(d.Layout.Relations, func(i, j int) bool {
			left, right := d.Layout.Relations[i], d.Layout.Relations[j]
			return left.Type+"\x00"+left.From+"\x00"+left.To < right.Type+"\x00"+right.From+"\x00"+right.To
		})
		sort.Slice(d.Layout.Recipes, func(i, j int) bool { return d.Layout.Recipes[i].ID < d.Layout.Recipes[j].ID })
	}
	sort.Slice(d.Rules, func(i, j int) bool { return d.Rules[i].ID < d.Rules[j].ID })
}

func (d Document) Validate() error {
	if strings.TrimSpace(d.ID) == "" || strings.TrimSpace(d.Version) == "" || strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.Summary) == "" {
		return errors.New("document id, version, title and summary are required")
	}
	switch d.DocumentSchema {
	case LayoutSchema:
		if d.Kind != "layout" || d.Layout == nil || d.Element != nil || len(d.Rules) != 0 {
			return errors.New("layout_doc.v1 requires exactly one layout body")
		}
		if err := validateLayout(*d.Layout); err != nil {
			return err
		}
	case ElementSchema:
		if d.Kind != "element" || d.Element == nil || d.Layout != nil || len(d.Rules) != 0 {
			return errors.New("element_doc.v1 requires exactly one element body")
		}
		if strings.TrimSpace(d.Element.Purpose) == "" || len(d.Element.CatalogComponents) == 0 {
			return errors.New("element purpose and catalogComponents are required")
		}
	case RuleSchema:
		if d.Kind != "rule" || d.Layout != nil || d.Element != nil || len(d.Rules) == 0 {
			return errors.New("atomic_rule_doc.v1 requires at least one rule")
		}
		seen := map[string]struct{}{}
		for i, rule := range d.Rules {
			if err := validateRule(rule); err != nil {
				return fmt.Errorf("rules[%d]: %w", i, err)
			}
			if _, ok := seen[rule.ID]; ok {
				return fmt.Errorf("duplicate rule id %q", rule.ID)
			}
			seen[rule.ID] = struct{}{}
		}
	default:
		return fmt.Errorf("unsupported document schema %q", d.DocumentSchema)
	}
	return nil
}

func validateLayout(body LayoutBody) error {
	if len(body.Nodes) == 0 {
		return errors.New("layout requires at least one node")
	}
	seen := map[string]struct{}{}
	for _, node := range body.Nodes {
		if strings.TrimSpace(node.ID) == "" || strings.TrimSpace(node.Type) == "" {
			return errors.New("layout node id and type are required")
		}
		if _, ok := seen[node.ID]; ok {
			return fmt.Errorf("duplicate node id %q", node.ID)
		}
		seen[node.ID] = struct{}{}
	}
	for _, node := range body.Nodes {
		if node.ParentID != "" {
			if _, ok := seen[node.ParentID]; !ok {
				return fmt.Errorf("node %q has missing parent %q", node.ID, node.ParentID)
			}
		}
	}
	for _, relation := range body.Relations {
		if strings.TrimSpace(relation.Type) == "" || strings.TrimSpace(relation.From) == "" || strings.TrimSpace(relation.To) == "" {
			return errors.New("relation type, from and to are required")
		}
		if !validStrength(relation.Strength) {
			return fmt.Errorf("relation has invalid strength %q", relation.Strength)
		}
	}
	return nil
}

func validateRule(rule AtomicRule) error {
	if strings.TrimSpace(rule.ID) == "" || strings.TrimSpace(rule.Target) == "" || strings.TrimSpace(rule.Effect) == "" || len(rule.SourceTrace) == 0 {
		return errors.New("id, target, effect and sourceTrace are required")
	}
	if !validStrength(rule.Strength) {
		return fmt.Errorf("invalid strength %q", rule.Strength)
	}
	return nil
}

func validStrength(value Strength) bool {
	switch value {
	case StrengthRequired, StrengthRecommended, StrengthOptional, StrengthForbidden:
		return true
	}
	return false
}
