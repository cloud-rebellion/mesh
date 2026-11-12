// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package relate

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/graph"
)

type entityName struct{ id, name string }

// EntityEvidence corroborates a ranked candidate with an explicit, unambiguous
// canonical entity name. readCurrent must validate the live file's identity,
// entity type and the caller's read permission before returning its title.
// Ordinary short words such as Mesh, Hash and Stage alone do not qualify.
// No rank, common word, inferred alias or inaccessible entity becomes evidence.
func EntityEvidence(source string, g *graph.Graph, readCurrent func(*graph.Node) (string, bool)) func(string, string) bool {
	none := func(string, string) bool { return false }
	if g == nil || readCurrent == nil {
		return none
	}
	source = entityText(source)
	nodes := g.Nodes()
	counts := make(map[string]int)
	for _, node := range nodes {
		if node.Kind == "note" && node.Attrs["type"] == "entity" {
			counts[strings.ToLower(entityText(node.Label))]++
		}
	}
	var matched []entityName
	for _, node := range nodes {
		if node.Kind != "note" || node.Attrs["type"] != "entity" {
			continue
		}
		name := entityText(node.Label)
		if counts[strings.ToLower(name)] != 1 || !distinctiveEntityName(name) || !entityMention(source, name) {
			continue
		}
		current, ok := readCurrent(node)
		if !ok || entityText(current) != name {
			continue
		}
		matched = append(matched, entityName{id: node.NoteID, name: name})
	}
	return func(targetID, text string) bool {
		text = entityText(text)
		for _, entity := range matched {
			if targetID == entity.id || entityMention(text, entity.name) {
				return true
			}
		}
		return false
	}
}

func entityText(text string) string { return strings.Join(strings.Fields(text), " ") }

func distinctiveEntityName(name string) bool {
	words := strings.Fields(name)
	if len(words) > 1 {
		if len(words) > 5 {
			return false
		}
		for _, word := range words {
			first, _ := utf8.DecodeRuneInString(word)
			if !unicode.IsUpper(first) || utf8.RuneCountInString(word) < 2 {
				return false
			}
		}
		return true
	}
	previous := rune(0)
	for _, r := range name {
		if unicode.IsLower(previous) && unicode.IsUpper(r) {
			return true
		}
		previous = r
	}
	return false
}

func entityMention(text, name string) bool {
	for offset := 0; offset < len(text); {
		position := strings.Index(text[offset:], name)
		if position < 0 {
			return false
		}
		position += offset
		end := position + len(name)
		before, after := rune(0), rune(0)
		if position > 0 {
			before, _ = utf8.DecodeLastRuneInString(text[:position])
		}
		if end < len(text) {
			after, _ = utf8.DecodeRuneInString(text[end:])
		}
		word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }
		if !word(before) && !word(after) {
			return true
		}
		offset = end
	}
	return false
}
