// Copyright 2019 Go About B.V.
// Parts adapted from kustomize, Copyright 2019 The Kubernetes Authors.
// Licensed under the Apache License, Version 2.0.

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pkg/errors"
	sopscommon "go.mozilla.org/sops/cmd/sops/common"
	sopsdecrypt "go.mozilla.org/sops/decrypt"
	"gopkg.in/yaml.v2"
)

const apiVersion = "goabout.com/v1beta1"
const kind = "SopsSecret"

var utf8bom = []byte{0xEF, 0xBB, 0xBF}

type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

type ObjectMeta struct {
	Name        string            `json:"name" yaml:"name"`
	Namespace   string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

type SopsSecret struct {
	TypeMeta              `json:",inline" yaml:",inline"`
	ObjectMeta            `json:"metadata" yaml:"metadata"`
	EnvSources            []string `json:"envs" yaml:"envs"`
	FileSources           []string `json:"files" yaml:"files"`
	Behavior              string   `json:"behavior,omitempty" yaml:"behavior,omitempty"`
	DisableNameSuffixHash bool     `json:"disableNameSuffixHash,omitempty" yaml:"disableNameSuffixHash,omitempty"`
	Type                  string   `json:"type,omitempty" yaml:"type,omitempty"`
}

type Secret struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Data       map[string]string `json:"data" yaml:"data"`
	Type       string            `json:"type,omitempty" yaml:"type,omitempty"`
}

type Pair struct {
	key   string
	value string
}

func main() {
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: SopsSecret FILE")
		os.Exit(1)
	}

	output, err := generateSecret(os.Args[1])
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(output)
}

func generateSecret(fn string) (string, error) {
	input, err := readInput(fn)
	if err != nil {
		return "", err
	}
	data, err := parseInput(input)
	if err != nil {
		return "", err
	}

	if !input.DisableNameSuffixHash {
		input.ObjectMeta.Annotations["kustomize.config.k8s.io/needs-hash"] = "true"
	}
	if input.Behavior != "" {
		input.ObjectMeta.Annotations["kustomize.config.k8s.io/behavior"] = input.Behavior
	}

	secret := Secret{
		TypeMeta: TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: input.ObjectMeta,
		Data:       data,
		Type:       input.Type,
	}
	output, err := yaml.Marshal(secret)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func readInput(fn string) (SopsSecret, error) {
	input := SopsSecret{
		TypeMeta: TypeMeta{},
		ObjectMeta: ObjectMeta{
			Annotations: make(map[string]string),
		},
	}
	content, err := ioutil.ReadFile(fn)
	if err != nil {
		return input, err
	}
	err = yaml.Unmarshal(content, &input)
	if err != nil {
		return input, err
	}

	if input.APIVersion != apiVersion || input.Kind != kind {
		return input, errors.Errorf("input must be apiVersion %s, kind %s", apiVersion, kind)
	}
	if input.Name == "" {
		return input, errors.New("input must contain metadata.name value")
	}
	return input, nil
}

func parseInput(input SopsSecret) (map[string]string, error) {
	data := make(map[string]string)
	err := parseEnvSources(input.EnvSources, data)
	if err != nil {
		return nil, err
	}
	err = parseFileSources(input.FileSources, data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func parseEnvSources(sources []string, data map[string]string) error {
	for _, source := range sources {
		err := parseEnvSource(source, data)
		if err != nil {
			return errors.Wrapf(err, "env source %v", source)
		}
	}
	return nil
}

func parseEnvSource(source string, data map[string]string) error {
	content, err := ioutil.ReadFile(source)
	if err != nil {
		return err
	}

	format := formatForPath(source)
	decrypted, err := sopsdecrypt.Data(content, format)
	if err != nil {
		return err
	}

	switch format {
	case "dotenv":
		err = parseDotEnvContent(decrypted, data)
	case "yaml":
		err = parseYamlContent(decrypted, data)
	case "json":
		err = parseJsonContent(decrypted, data)
	default:
		err = errors.New("unknown file format, use dotenv, yaml or json")
	}
	if err != nil {
		return err
	}

	return nil
}

func parseDotEnvContent(content []byte, data map[string]string) error {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	lineNum := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if lineNum == 0 {
			line = bytes.TrimPrefix(line, utf8bom)
		}
		err := parseEnvLine(line, data)
		if err != nil {
			return errors.Wrapf(err, "line %d", lineNum)
		}
		lineNum++
	}
	return nil
}

func parseEnvLine(line []byte, data map[string]string) error {
	if !utf8.Valid(line) {
		return fmt.Errorf("invalid UTF-8 bytes: %v", string(line))
	}

	line = bytes.TrimLeftFunc(line, unicode.IsSpace)

	if len(line) == 0 || line[0] == '#' {
		return nil
	}

	pair := strings.SplitN(string(line), "=", 2)
	if len(pair) != 2 {
		return fmt.Errorf("requires value: %v", string(line))
	}

	data[pair[0]] = base64.StdEncoding.EncodeToString([]byte(pair[1]))
	return nil
}

func parseYamlContent(content []byte, data map[string]string) error {
	d := make(map[string]string)
	err := yaml.Unmarshal(content, d)
	if err != nil {
		return err
	}
	for k, v := range d {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return nil
}

func parseJsonContent(content []byte, data map[string]string) error {
	d := make(map[string]string)
	err := json.Unmarshal(content, &d)
	if err != nil {
		return err
	}
	for k, v := range d {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return nil
}

func parseFileSources(sources []string, data map[string]string) error {
	for _, source := range sources {
		err := parseFileSource(source, data)
		if err != nil {
			return errors.Wrapf(err, "file source %v", source)
		}
	}
	return nil
}

func parseFileSource(source string, data map[string]string) error {
	key, fn, err := parseFileName(source)
	if err != nil {
		return err
	}

	content, err := ioutil.ReadFile(fn)
	if err != nil {
		return err
	}

	decrypted, err := sopsdecrypt.Data(content, formatForPath(source))
	if err != nil {
		return err
	}

	data[key] = base64.StdEncoding.EncodeToString(decrypted)
	return nil
}

func parseFileName(source string) (string, string, error) {
	sepNum := strings.Count(source, "=")
	switch {
	case sepNum == 0:
		return path.Base(source), source, nil
	case sepNum == 1 && strings.HasPrefix(source, "="):
		return "", "", fmt.Errorf("key name for file path %v missing", strings.TrimPrefix(source, "="))
	case sepNum == 1 && strings.HasSuffix(source, "="):
		return "", "", fmt.Errorf("file path for key name %v missing", strings.TrimSuffix(source, "="))
	case sepNum > 1:
		return "", "", errors.New("key names or file paths cannot contain '='")
	default:
		components := strings.Split(source, "=")
		return components[0], components[1], nil
	}
}

func formatForPath(path string) string {
	if sopscommon.IsYAMLFile(path) {
		return "yaml"
	} else if sopscommon.IsJSONFile(path) {
		return "json"
	} else if sopscommon.IsEnvFile(path) {
		return "dotenv"
	}
	return "binary"
}