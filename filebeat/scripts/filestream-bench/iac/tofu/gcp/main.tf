terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
    http = {
      source  = "hashicorp/http"
      version = "~> 3.0"
    }
  }
}

variable "instances" {
  description = "Benchmark instance names (slugs)"
  type        = set(string)
}

variable "gcp_project" {
  description = "GCP project ID"
  type        = string
}

variable "gcp_region" {
  type    = string
  default = "us-central1"
}

variable "gcp_zone" {
  type    = string
  default = "us-central1-a"
}

variable "owner" {
  description = "Resource name prefix and value of the project label"
  type        = string
}

variable "labels" {
  description = "Extra labels for every resource, for orgs that require them"
  type        = map(string)
  default     = {}
}

variable "operator_cidr" {
  description = "CIDR allowed to reach SSH; defaults to the caller's public IP"
  type        = string
  default     = ""
}

variable "ssh_public_key_file" {
  type    = string
  default = "~/.ssh/id_rsa.pub"
}

variable "agent_image" {
  description = "Boot image; must match the machine type architecture"
  type        = string
  default     = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
}

variable "agent_machine_type" {
  description = "E2 is GCP's broadest fleet; for arm64 use c4a-highcpu-4 with the arm64 image and hyperdisk-balanced"
  type        = string
  default     = "e2-highcpu-4"
}

variable "agent_disk_type" {
  type    = string
  default = "pd-balanced"
}

variable "disk_gb" {
  description = "Boot disk size; must hold the generated log files"
  type        = number
  default     = 50
}

provider "google" {
  project = var.gcp_project
  region  = var.gcp_region
  zone    = var.gcp_zone
}

data "http" "my_ip" {
  count = var.operator_cidr == "" ? 1 : 0
  url   = "https://checkip.amazonaws.com"
}

locals {
  prefix         = "${var.owner}-fsbench"
  ssh_public_key = trimspace(file(pathexpand(var.ssh_public_key_file)))
  operator_cidr  = var.operator_cidr != "" ? var.operator_cidr : "${chomp(data.http.my_ip[0].response_body)}/32"
  labels         = merge(var.labels, { project = var.owner, ephemeral = "true" })
  user_data      = templatefile("${path.module}/../cloud-init.tftpl", { ssh_public_key = local.ssh_public_key })
}

resource "google_compute_network" "bench" {
  name                    = "${local.prefix}-net"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "bench" {
  name          = "${local.prefix}-subnet"
  network       = google_compute_network.bench.id
  ip_cidr_range = "10.82.0.0/24"
  region        = var.gcp_region
}

resource "google_compute_firewall" "operator" {
  name    = "${local.prefix}-allow-operator"
  network = google_compute_network.bench.name

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
  source_ranges = [local.operator_cidr]
  target_tags   = [local.prefix]
}

resource "google_compute_instance" "agent" {
  for_each = var.instances

  name         = "${local.prefix}-${each.key}"
  machine_type = var.agent_machine_type
  zone         = var.gcp_zone
  tags         = [local.prefix]
  labels       = local.labels

  boot_disk {
    initialize_params {
      image  = var.agent_image
      size   = var.disk_gb
      type   = var.agent_disk_type
      labels = local.labels
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.bench.id
    access_config {}
  }

  metadata = {
    user-data = local.user_data
  }
}

output "instance_ips" {
  value = { for k, v in google_compute_instance.agent : k => v.network_interface[0].access_config[0].nat_ip }
}

output "instance_internal_ips" {
  value = { for k, v in google_compute_instance.agent : k => v.network_interface[0].network_ip }
}

output "instance_hostnames" {
  value = { for k, v in google_compute_instance.agent : k => v.name }
}
