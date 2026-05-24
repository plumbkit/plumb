package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var hclSrc = []byte(`terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = "us-east-1"
}

variable "region" {
  type    = string
  default = "us-east-1"
}

locals {
  service = "web"
  owner   = "team-a"
}

resource "aws_instance" "web" {
  ami           = "ami-12345678"
  instance_type = "t3.micro"
}

data "aws_ami" "ubuntu" {
  most_recent = true
}

module "vpc" {
  source = "./modules/vpc"
}

output "instance_ip" {
  value = aws_instance.web.public_ip
}
`)

func TestHCL_KindsExtracted(t *testing.T) {
	nodes, _, err := NewHCL().Extract(context.Background(), "infra/main.tf", hclSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindVariable, "region"},
		{topology.KindConstant, "service"},
		{topology.KindConstant, "owner"},
		{topology.KindType, "aws"},              // provider
		{topology.KindType, "aws_instance.web"}, // resource
		{topology.KindType, "data.aws_ami.ubuntu"},
		{topology.KindImport, "vpc"},           // module
		{topology.KindConstant, "instance_ip"}, // output
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestHCL_TerraformSettingsBlockSkipped(t *testing.T) {
	nodes, _, err := NewHCL().Extract(context.Background(), "main.tf", hclSrc)
	if err != nil {
		t.Fatal(err)
	}
	// The `terraform { … }` settings block has no label and is not a searchable
	// declaration; it must not appear as a type node.
	if slices.Contains(names(nodes, topology.KindType), "terraform") || slices.Contains(names(nodes, topology.KindType), "") {
		t.Errorf("terraform settings block should be skipped; types=%v", names(nodes, topology.KindType))
	}
}

func TestHCL_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewHCL().Extract(context.Background(), "main.tf", hclSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindType && n.Name == "aws_instance.web" {
			if n.EndLine <= n.StartLine {
				t.Errorf("resource EndLine=%d should exceed StartLine=%d", n.EndLine, n.StartLine)
			}
			return
		}
	}
	t.Fatal("aws_instance.web resource node not found")
}

func TestHCL_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("# just a comment\n# more\n")} {
		nodes, edges, err := NewHCL().Extract(context.Background(), "e.tf", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestHCL_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewHCL().Extract(context.Background(), "infra/main.tf", hclSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "hcl" {
			t.Errorf("node %q language=%q, want hcl", n.Name, n.Language)
		}
		if n.Path != "infra/main.tf" {
			t.Errorf("node %q path=%q, want infra/main.tf", n.Name, n.Path)
		}
	}
}

func TestHCL_Extensions(t *testing.T) {
	exts := NewHCL().Extensions()
	for _, want := range []string{".tf", ".tfvars", ".hcl"} {
		if !slices.Contains(exts, want) {
			t.Errorf("%s missing from HCL Extensions()", want)
		}
	}
}
