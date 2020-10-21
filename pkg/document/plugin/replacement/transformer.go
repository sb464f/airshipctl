// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package replacement

import (
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/kustomize/api/k8sdeps/kunstruct"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	airshipv1 "opendev.org/airship/airshipctl/pkg/api/v1alpha1"
	"opendev.org/airship/airshipctl/pkg/document/plugin/kyamlutils"
	plugtypes "opendev.org/airship/airshipctl/pkg/document/plugin/types"
)

var (
	pattern = regexp.MustCompile(`(\S+)\[(\S+)=(\S+)\]`)
	// substring substitutions are appended to paths as: ...%VARNAME%
	substringPatternRegex = regexp.MustCompile(`(.+)%(\S+)%$`)
)

const (
	dotReplacer = "$$$$"
)

var _ plugtypes.Plugin = &plugin{}

type plugin struct {
	*airshipv1.ReplacementTransformer
}

// New creates new instance of the plugin
func New(obj map[string]interface{}) (plugtypes.Plugin, error) {
	cfg := &airshipv1.ReplacementTransformer{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, cfg)
	if err != nil {
		return nil, err
	}
	p := &plugin{ReplacementTransformer: cfg}
	for _, r := range p.Replacements {
		if r.Source == nil {
			return nil, ErrBadConfiguration{Msg: "`from` must be specified in one replacement"}
		}
		if r.Target == nil {
			return nil, ErrBadConfiguration{Msg: "`to` must be specified in one replacement"}
		}
		if r.Source.ObjRef != nil && r.Source.Value != "" {
			return nil, ErrBadConfiguration{Msg: "only one of fieldref and value is allowed in one replacement"}
		}
	}
	return p, nil
}

func (p *plugin) Run(in io.Reader, out io.Writer) error {
	data, err := ioutil.ReadAll(in)
	if err != nil {
		return err
	}

	kf := kunstruct.NewKunstructuredFactoryImpl()
	rf := resource.NewFactory(kf)
	resources, err := rf.SliceFromBytes(data)
	if err != nil {
		return err
	}

	rm := resmap.New()
	for _, r := range resources {
		if err = rm.Append(r); err != nil {
			return err
		}
	}

	if err = p.Transform(rm); err != nil {
		return err
	}

	result, err := rm.AsYaml()
	if err != nil {
		return err
	}
	fmt.Fprint(out, string(result))
	return nil
}

// Transform resources using configured replacements
func (p *plugin) Transform(m resmap.ResMap) error {
	var err error
	for _, r := range p.Replacements {
		var replacement interface{}
		if r.Source.ObjRef != nil {
			replacement, err = getReplacement(m, r.Source.ObjRef, r.Source.FieldRef)
			if err != nil {
				return err
			}
		}
		if r.Source.Value != "" {
			replacement = r.Source.Value
		}
		if err = substitute(m, r.Target, replacement); err != nil {
			return err
		}
	}
	return nil
}

func (p *plugin) Filter(items []*yaml.RNode) ([]*yaml.RNode, error) {
	for _, r := range p.Replacements {
		val, err := getValue(items, r.Source)
		if err != nil {
			return nil, err
		}

		if err := replace(items, r.Target, val); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func getValue(items []*yaml.RNode, source *types.ReplSource) (*yaml.RNode, error) {
	if source.Value != "" {
		return yaml.NewScalarRNode(source.Value), nil
	}
	sources, err := kyamlutils.DocumentSelector{}.
		ByAPIVersion(source.ObjRef.APIVersion).
		ByGVK(source.ObjRef.Group, source.ObjRef.Version, source.ObjRef.Kind).
		ByName(source.ObjRef.Name).
		ByNamespace(source.ObjRef.Namespace).
		Filter(items)
	if err != nil {
		return nil, err
	}

	if len(sources) > 1 {
		return nil, ErrMultipleResources{ObjRef: source.ObjRef}
	}
	if len(sources) == 0 {
		return nil, ErrSourceNotFound{ObjRef: source.ObjRef}
	}

	path := fmt.Sprintf("{.%s.%s}", yaml.MetadataField, yaml.NameField)
	if source.FieldRef != "" {
		path = source.FieldRef
	}
	return sources[0].Pipe(kyamlutils.JSONPathFilter{Path: path})
}

func mutateField(rnSource *yaml.RNode) func([]*yaml.RNode) error {
	return func(rns []*yaml.RNode) error {
		for _, rn := range rns {
			rn.SetYNode(rnSource.YNode())
		}
		return nil
	}
}

func replace(items []*yaml.RNode, target *types.ReplTarget, value *yaml.RNode) error {
	targets, err := kyamlutils.DocumentSelector{}.
		ByGVK(target.ObjRef.Group, target.ObjRef.Version, target.ObjRef.Kind).
		ByName(target.ObjRef.Name).
		ByNamespace(target.ObjRef.Namespace).
		Filter(items)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return ErrTargetNotFound{ObjRef: target.ObjRef}
	}
	for _, tgt := range targets {
		for _, fieldRef := range target.FieldRefs {
			// fieldref can contain substring pattern for regexp - we need to get it
			groups := substringPatternRegex.FindStringSubmatch(fieldRef)
			// if there is no substring pattern
			if len(groups) != 3 {
				filter := kyamlutils.JSONPathFilter{Path: fieldRef, Mutator: mutateField(value), Create: true}
				if _, err := tgt.Pipe(filter); err != nil {
					return err
				}
				continue
			}

			if err := substituteSubstring(tgt, groups[1], groups[2], value); err != nil {
				return err
			}
		}
	}
	return nil
}

func substituteSubstring(tgt *yaml.RNode, fieldRef, substringPattern string, value *yaml.RNode) error {
	if err := yaml.ErrorIfInvalid(value, yaml.ScalarNode); err != nil {
		return err
	}
	curVal, err := tgt.Pipe(kyamlutils.JSONPathFilter{Path: fieldRef})
	if yaml.IsMissingOrError(curVal, err) {
		return err
	}
	switch curVal.YNode().Kind {
	case yaml.ScalarNode:
		p := regexp.MustCompile(substringPattern)
		curVal.YNode().Value = p.ReplaceAllString(yaml.GetValue(curVal), yaml.GetValue(value))

	case yaml.SequenceNode:
		items, err := curVal.Elements()
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := yaml.ErrorIfInvalid(item, yaml.ScalarNode); err != nil {
				return err
			}
			p := regexp.MustCompile(substringPattern)
			item.YNode().Value = p.ReplaceAllString(yaml.GetValue(item), yaml.GetValue(value))
		}
	default:
		return ErrPatternSubstring{Msg: fmt.Sprintf("value identified by %s expected to be string", fieldRef)}
	}
	return nil
}

func getReplacement(m resmap.ResMap, objRef *types.Target, fieldRef string) (interface{}, error) {
	s := types.Selector{
		Gvk:       objRef.Gvk,
		Name:      objRef.Name,
		Namespace: objRef.Namespace,
	}
	resources, err := m.Select(s)
	if err != nil {
		return nil, err
	}
	if len(resources) > 1 {
		resList := make([]string, len(resources))
		for i := range resources {
			resList[i] = resources[i].String()
		}
		return nil, ErrMultipleResources{ObjRef: objRef}
	}
	if len(resources) == 0 {
		return nil, ErrSourceNotFound{ObjRef: objRef}
	}
	if fieldRef == "" {
		fieldRef = "metadata.name"
	}
	return resources[0].GetFieldValue(fieldRef)
}

func substitute(m resmap.ResMap, to *types.ReplTarget, replacement interface{}) error {
	resources, err := m.Select(*to.ObjRef)
	if err != nil {
		return err
	}
	if len(resources) == 0 {
		return ErrTargetNotFound{ObjRef: to.ObjRef}
	}
	for _, r := range resources {
		for _, p := range to.FieldRefs {
			// TODO (dukov) rework this using k8s.io/client-go/util/jsonpath
			parts := strings.Split(p, "[")
			var tmp []string
			for _, part := range parts {
				if strings.Contains(part, "]") {
					filter := strings.Split(part, "]")
					filter[0] = strings.ReplaceAll(filter[0], ".", dotReplacer)
					part = strings.Join(filter, "]")
				}
				tmp = append(tmp, part)
			}
			p = strings.Join(tmp, "[")
			// Exclude substring portion from dot replacer
			// substring can contain IP or any dot separated string
			substringPattern := ""
			p, substringPattern = extractSubstringPattern(p)

			pathSlice := strings.Split(p, ".")
			// append back the extracted substring
			if len(substringPattern) > 0 {
				pathSlice[len(pathSlice)-1] = pathSlice[len(pathSlice)-1] + "%" +
					substringPattern + "%"
			}
			for i, part := range pathSlice {
				pathSlice[i] = strings.ReplaceAll(part, dotReplacer, ".")
			}
			if err := updateField(r.Map(), pathSlice, replacement); err != nil {
				return err
			}
		}
	}
	return nil
}

func getFirstPathSegment(path string) (field string, key string, value string, isArray bool) {
	groups := pattern.FindStringSubmatch(path)
	if len(groups) != 4 {
		return path, "", "", false
	}
	return groups[1], groups[2], groups[3], groups[2] != ""
}

func updateField(m interface{}, pathToField []string, replacement interface{}) error {
	if len(pathToField) == 0 {
		return nil
	}

	switch typedM := m.(type) {
	case map[string]interface{}:
		return updateMapField(typedM, pathToField, replacement)
	case []interface{}:
		return updateSliceField(typedM, pathToField, replacement)
	default:
		return ErrTypeMismatch{Actual: typedM, Expectation: "is not expected be a primitive type"}
	}
}

// Extract the substring pattern (if present) from the target path spec
func extractSubstringPattern(path string) (extractedPath string, substringPattern string) {
	groups := substringPatternRegex.FindStringSubmatch(path)
	if len(groups) != 3 {
		return path, ""
	}
	return groups[1], groups[2]
}

// replaces substring in a string if pattern applies
func processString(field string, substringPattern string, replacement string) string {
	pattern := regexp.MustCompile(substringPattern)
	return pattern.ReplaceAllString(field, replacement)
}

// replaces substring in any string in the array if pattern applies
func processArray(tgt []string, pattern string, replacement string) []string {
	result := make([]string, 0, len(tgt))
	for _, field := range tgt {
		result = append(result, processString(field, pattern, replacement))
	}
	return result
}

// converts array of interfaces to array of strings
func convertToStrings(iFaces []interface{}) ([]string, bool) {
	result := []string{}
	for _, val := range iFaces {
		if str, ok := val.(string); ok {
			result = append(result, str)
		}
	}
	return result, (len(result) != 0)
}

// apply a substring substitution based on a pattern
func applySubstringPattern(target interface{}, replacement interface{},
	substringPattern string) (regexedReplacement interface{}, err error) {
	// no regex'ing needed if there is no substringPattern
	if substringPattern == "" {
		return replacement, nil
	}

	// if replacement is numeric, convert it to a string for the sake of
	// substring substitution.
	var replacementString string
	switch t := replacement.(type) {
	case uint, uint8, uint16, uint32, uint64, int, int8, int16, int32, int64:
		replacementString = fmt.Sprint(t)
	case string:
		replacementString = t
	default:
		return nil, ErrPatternSubstring{Msg: "pattern-based substitution can only be applied " +
			"with string or numeric replacement values"}
	}

	switch reflect.TypeOf(target).Kind() {
	case reflect.String:
		return processString(target.(string), substringPattern, replacementString), nil
	case reflect.Slice:
		if ifaceArray, ok := target.([]interface{}); ok {
			if strArray, ok := convertToStrings(ifaceArray); ok {
				return processArray(strArray, substringPattern, replacementString), nil
			}
		}
		if strArray, ok := target.([]string); ok {
			return processArray(strArray, substringPattern, replacementString), nil
		}
	}
	return nil, ErrPatternSubstring{
		Msg: "pattern-based substitution can only be applied to string " +
			"or array of strings target fields",
	}
}

func updateMapField(m map[string]interface{}, pathToField []string, replacement interface{}) error {
	path, key, value, isArray := getFirstPathSegment(pathToField[0])

	path, substringPattern := extractSubstringPattern(path)

	v, found := m[path]
	if !found {
		m[path] = make(map[string]interface{})
		v = m[path]
	}

	if v == nil {
		return ErrTypeMismatch{Actual: v, Expectation: "is not expected be nil"}
	}

	if len(pathToField) == 1 {
		if !isArray {
			renderedRepl, err := applySubstringPattern(m[path], replacement, substringPattern)
			if err != nil {
				return err
			}
			m[path] = renderedRepl
			return nil
		}
		switch typedV := v.(type) {
		case []interface{}:
			for i, item := range typedV {
				typedItem, ok := item.(map[string]interface{})
				if !ok {
					return ErrTypeMismatch{Actual: item, Expectation: fmt.Sprintf("is expected to be %T", typedItem)}
				}
				if actualValue, ok := typedItem[key]; ok {
					if value == actualValue {
						typedV[i] = replacement
						return nil
					}
				}
			}
		default:
			return ErrTypeMismatch{Actual: typedV, Expectation: "is not expected be a primitive type"}
		}
	}

	if isArray {
		return updateField(v, pathToField, replacement)
	}
	return updateField(v, pathToField[1:], replacement)
}

func updateSliceField(m []interface{}, pathToField []string, replacement interface{}) error {
	if len(pathToField) == 0 {
		return nil
	}
	_, key, value, isArray := getFirstPathSegment(pathToField[0])

	if isArray {
		for _, item := range m {
			typedItem, ok := item.(map[string]interface{})
			if !ok {
				return ErrTypeMismatch{Actual: item, Expectation: fmt.Sprintf("is expected to be %T", typedItem)}
			}
			if actualValue, ok := typedItem[key]; ok {
				if value == actualValue {
					return updateField(typedItem, pathToField[1:], replacement)
				}
			}
		}
		return nil
	}

	index, err := strconv.Atoi(pathToField[0])
	if err != nil {
		return err
	}

	if len(m) < index || index < 0 {
		return ErrIndexOutOfBound{Index: index, Length: len(m)}
	}
	if len(pathToField) == 1 {
		m[index] = replacement
		return nil
	}
	return updateField(m[index], pathToField[1:], replacement)
}
