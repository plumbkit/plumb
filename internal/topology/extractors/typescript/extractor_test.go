package typescript

import (
	"context"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var tsSrc = []byte(`import React from 'react'
import { useState } from 'react'
import axios from 'axios'

export class UserService {
  async getUser(id: string): Promise<User> {
    return axios.get('/users/' + id)
  }

  createUser(data: UserData): Promise<User> {
    return axios.post('/users', data)
  }
}

export interface User {
  id: string
  name: string
}

export type UserId = string

export async function fetchUsers(): Promise<User[]> {
  return axios.get('/users')
}

export const formatUser = (u: User) => u.name

const handler = async (req: Request) => {
  return fetchUsers()
}
`)

func TestExtract_ClassMethodsImports(t *testing.T) {
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "service.ts", tsSrc)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	byKind := map[string][]string{}
	for _, n := range nodes {
		byKind[string(n.Kind)] = append(byKind[string(n.Kind)], n.Name)
	}

	cases := []struct{ kind, name string }{
		{"class", "UserService"},
		{"method", "getUser"},
		{"method", "createUser"},
		{"type", "User"},   // interface
		{"type", "UserId"}, // type alias
		{"function", "fetchUsers"},
		{"function", "formatUser"},
		{"import", "react"},
		{"import", "axios"},
	}
	for _, c := range cases {
		if !slices.Contains(byKind[c.kind], c.name) {
			t.Errorf("kind=%q name=%q not found; got %v", c.kind, c.name, byKind[c.kind])
		}
	}
	if len(edges) == 0 {
		t.Error("expected containment edges for class methods, got 0")
	}
}

func TestExtract_EmptyFile(t *testing.T) {
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "empty.ts", []byte(""))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected 0 nodes for empty file, got %d", len(nodes))
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for empty file, got %d", len(edges))
	}
}

func TestExtract_MinifiedFileSkipped(t *testing.T) {
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "bundle.min.js", []byte("function f(){}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 || len(edges) != 0 {
		t.Error("minified file should return zero nodes and edges")
	}
}

func TestExtract_TestBlocks(t *testing.T) {
	src := []byte(`import { describe, it, expect } from 'vitest'

describe("UserService", () => {
  it("fetches users", async () => {
    expect(true).toBe(true)
  })
})

test("standalone test", () => {
  expect(1).toBe(1)
})
`)
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "service.test.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	testNames := []string{}
	for _, n := range nodes {
		if n.Kind == topology.KindTest {
			testNames = append(testNames, n.Name)
		}
	}
	if len(testNames) == 0 {
		t.Error("expected test nodes from describe/it/test blocks")
	}
}

func TestExtract_ESModuleImport(t *testing.T) {
	src := []byte("import express from 'express'\nimport { Router } from 'express'\n")
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "app.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	var importNames []string
	for _, n := range nodes {
		if n.Kind == topology.KindImport {
			importNames = append(importNames, n.Name)
		}
	}
	if !slices.Contains(importNames, "express") {
		t.Errorf("expected import 'express'; got %v", importNames)
	}
}

func TestExtract_CommonJSRequire(t *testing.T) {
	src := []byte("const path = require('path')\nconst fs = require('fs')\n")
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "app.js", src)
	if err != nil {
		t.Fatal(err)
	}
	var importNames []string
	for _, n := range nodes {
		if n.Kind == topology.KindImport {
			importNames = append(importNames, n.Name)
		}
	}
	if !slices.Contains(importNames, "path") {
		t.Errorf("expected import 'path'; got %v", importNames)
	}
}

func TestExtract_ExpressRoute(t *testing.T) {
	src := []byte(`const express = require('express')
const app = express()

app.get('/users', async (req, res) => {
  res.json([])
})

app.post('/users', (req, res) => {
  res.status(201).json({})
})
`)
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "server.js", src)
	if err != nil {
		t.Fatal(err)
	}
	routeNames := []string{}
	for _, n := range nodes {
		if n.Kind == topology.KindFunction && (hasPrefix(n.Name, "GET_") || hasPrefix(n.Name, "POST_")) {
			routeNames = append(routeNames, n.Name)
		}
	}
	if len(routeNames) == 0 {
		t.Error("expected route entry nodes from app.get/post")
	}
}

func TestExtract_ContainmentEdge(t *testing.T) {
	src := []byte(`class MyClass {
  myMethod() {
    return 42
  }
}
`)
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "c.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	_ = nodes
	hasContains := false
	for _, e := range edges {
		if e.Kind == topology.EdgeContains {
			hasContains = true
			if e.Confidence != 0.7 {
				t.Errorf("containment edge confidence=%v, want 0.7", e.Confidence)
			}
		}
	}
	if !hasContains {
		t.Error("expected containment edge for class method")
	}
}

func TestExtract_Extensions(t *testing.T) {
	ext := New()
	exts := ext.Extensions()
	required := []string{".tsx", ".jsx"}
	for _, r := range required {
		if !slices.Contains(exts, r) {
			t.Errorf("extension %q missing from Extensions()", r)
		}
	}
}

func TestExtract_LanguageAndPath(t *testing.T) {
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "pkg/foo.ts", tsSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "typescript" {
			t.Errorf("node %q has language=%q, want typescript", n.Name, n.Language)
		}
		if n.Path != "pkg/foo.ts" {
			t.Errorf("node %q has path=%q, want pkg/foo.ts", n.Name, n.Path)
		}
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
