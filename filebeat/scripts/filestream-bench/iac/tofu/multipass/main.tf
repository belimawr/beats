terraform {
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.0"
    }
    external = {
      source  = "hashicorp/external"
      version = "~> 2.0"
    }
  }
}

variable "instances" {
  description = "Benchmark instance names (slugs)"
  type        = set(string)
}

variable "owner" {
  description = "Prefix for VM names"
  type        = string
  default     = "fsbench"
}

variable "ssh_public_key_file" {
  description = "Path to SSH public key file to inject into the VMs"
  type        = string
  default     = "~/.ssh/id_rsa.pub"
}

variable "agent_cpus" {
  type    = number
  default = 4
}

variable "agent_memory" {
  type    = string
  default = "8G"
}

variable "agent_disk" {
  description = "Must hold the generated log files"
  type        = string
  default     = "50G"
}

variable "launch_timeout_seconds" {
  type    = number
  default = 600
}

locals {
  ssh_public_key = trimspace(file(pathexpand(var.ssh_public_key_file)))
}

resource "local_file" "cloud_init" {
  content  = templatefile("${path.module}/../cloud-init.tftpl", { ssh_public_key = local.ssh_public_key })
  filename = "${path.module}/cloud-init.yaml"
}

# The larstobi/multipass provider mis-parses "Launched: <name> in <zone>" from
# multipass 1.17+, so the VMs are managed with the multipass CLI directly.
resource "terraform_data" "agent" {
  for_each = var.instances

  input = {
    name       = "${var.owner}-${each.key}"
    cpus       = var.agent_cpus
    memory     = var.agent_memory
    disk       = var.agent_disk
    cloud_init = local_file.cloud_init.content
  }
  triggers_replace = {
    name       = "${var.owner}-${each.key}"
    cpus       = var.agent_cpus
    memory     = var.agent_memory
    disk       = var.agent_disk
    cloud_init = local_file.cloud_init.content
  }

  provisioner "local-exec" {
    command = "multipass launch lts --name ${var.owner}-${each.key} --cpus ${var.agent_cpus} --memory ${var.agent_memory} --disk ${var.agent_disk} --cloud-init ${abspath(local_file.cloud_init.filename)} --timeout ${var.launch_timeout_seconds}"
  }

  provisioner "local-exec" {
    when    = destroy
    command = "multipass delete --purge ${self.input.name}"
  }
}

data "external" "agent" {
  for_each = var.instances

  program    = ["sh", "-c", "printf '{\"ip\":\"%s\"}' \"$(multipass info ${var.owner}-${each.key} | awk '/^IPv4:/{print $2; exit}')\""]
  depends_on = [terraform_data.agent]
}

output "instance_ips" {
  value = { for k, v in data.external.agent : k => v.result.ip }
}

output "instance_internal_ips" {
  value = { for k, v in data.external.agent : k => v.result.ip }
}

output "instance_hostnames" {
  value = { for k in var.instances : k => "${var.owner}-${k}" }
}
