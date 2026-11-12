variable "my_ip" {
  description = "Your public IPv4 address, for example from `curl -s https://checkip.amazonaws.com`. The VM's security group allows SSH from this address only."
  type        = string

  validation {
    condition     = can(regex("^\\d{1,3}(\\.\\d{1,3}){3}$", var.my_ip)) && can(cidrnetmask("${var.my_ip}/32"))
    error_message = "my_ip must be a single IPv4 address, such as 203.0.113.7, without a prefix length."
  }
}

variable "region" {
  description = "AWS region of the lab."
  type        = string
  default     = "us-east-1"
}

variable "az_id" {
  description = "AZ ID (not name) of the VM and the S3 Express One Zone bucket. It must be an AZ that supports S3 Express One Zone. AZ IDs are the same in every account; AZ names aren't."
  type        = string
  default     = "use1-az4"
}

variable "name" {
  description = "Prefix of resource names. The AWS account ID is added to bucket names, so they're globally unique."
  type        = string
  default     = "awscache-lab"
}

variable "instance_type" {
  description = "EC2 instance type of the VM that runs the tests. The default is close to a CI runner VM (14 vCPUs)."
  type        = string
  default     = "c7i.4xlarge"
}

variable "root_volume_gb" {
  description = "Size of the VM's root volume, which also holds the build caches."
  type        = number
  default     = 100
}

variable "go_version" {
  description = "Go version to install on the VM."
  type        = string
  default     = "1.27.4"
}

variable "repo_url" {
  description = "Git repository to clone on the VM."
  type        = string
  default     = "https://github.com/tailscale/tb.git"
}

variable "repo_branch" {
  description = "Branch to check out on the VM. If it doesn't exist, the default branch is checked out."
  type        = string
  default     = "tomhjp/mid-object-cache"
}

variable "tailscale_ref" {
  description = "Commit, tag, or branch of github.com/tailscale/tailscale to check out on the VM, for the benchmark's Tailscale build cases. Pin a commit to compare runs over time."
  type        = string
  default     = "main"
}

variable "blob_expiry_days" {
  description = "Days after which the S3 Standard bucket's lifecycle rule deletes blobs. Each benchmark run writes under a new prefix, so old blobs are never read again."
  type        = number
  default     = 7
}
