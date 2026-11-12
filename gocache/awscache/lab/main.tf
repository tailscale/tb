# A lab environment to test and benchmark gocache/awscache: a DynamoDB
# table, an S3 Standard bucket, an S3 Express One Zone directory bucket, and
# an EC2 VM in the same AZ as the directory bucket to run the tests from.
# See README.md.

data "aws_caller_identity" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id

  table_name          = var.name
  bucket_name         = "${var.name}-${local.account_id}"
  express_bucket_name = "${var.name}-${local.account_id}--${var.az_id}--x-s3"
}

# Network: one public subnet in the AZ of the directory bucket, with
# gateway endpoints for S3, S3 Express, and DynamoDB, as in the ci VPC.

resource "aws_vpc" "lab" {
  cidr_block           = "10.88.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name = var.name
  }
}

resource "aws_subnet" "lab" {
  vpc_id                  = aws_vpc.lab.id
  cidr_block              = "10.88.0.0/24"
  availability_zone_id    = var.az_id
  map_public_ip_on_launch = true

  tags = {
    Name = var.name
  }
}

resource "aws_internet_gateway" "lab" {
  vpc_id = aws_vpc.lab.id

  tags = {
    Name = var.name
  }
}

resource "aws_route_table" "lab" {
  vpc_id = aws_vpc.lab.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.lab.id
  }

  tags = {
    Name = var.name
  }
}

resource "aws_route_table_association" "lab" {
  subnet_id      = aws_subnet.lab.id
  route_table_id = aws_route_table.lab.id
}

resource "aws_vpc_endpoint" "gateway" {
  for_each = toset(["s3", "s3express", "dynamodb"])

  vpc_id            = aws_vpc.lab.id
  service_name      = "com.amazonaws.${var.region}.${each.key}"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.lab.id]

  tags = {
    Name = "${var.name}-${each.key}"
  }
}

# Storage, configured like deploy/terraform/ci-gocache-aws in the corp repo,
# but easy to destroy.

resource "aws_dynamodb_table" "lab" {
  name         = local.table_name
  billing_mode = "PAY_PER_REQUEST"

  # NOTE: must match attrKey in gocache/awscache.
  hash_key = "k"

  attribute {
    name = "k"
    type = "S"
  }

  # NOTE: must match attrTTL in gocache/awscache.
  ttl {
    attribute_name = "ttl"
    enabled        = true
  }
}

resource "aws_s3_bucket" "blobs" {
  bucket        = local.bucket_name
  force_destroy = true
}

resource "aws_s3_bucket_public_access_block" "blobs" {
  bucket                  = aws_s3_bucket.blobs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "blobs" {
  bucket = aws_s3_bucket.blobs.id

  rule {
    id     = "expire-blobs"
    status = "Enabled"

    filter {}

    expiration {
      days = var.blob_expiry_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

# The S3 Express One Zone bucket, for the benchmark's express config.
# Directory buckets are private and use SSE-S3 by default. There's no
# lifecycle rule: the bench writes little to it, and force_destroy empties
# it at destroy time.
resource "aws_s3_directory_bucket" "express" {
  bucket        = local.express_bucket_name
  force_destroy = true

  location {
    name = var.az_id
    type = "AvailabilityZone"
  }
}

# Earlier versions of this lab called the bucket "mid". Keep labs created
# then from recreating it.
moved {
  from = aws_s3_directory_bucket.mid
  to   = aws_s3_directory_bucket.express
}

# The VM that runs the tests. Connect with SSH from var.my_ip (with EC2
# Instance Connect, which AL2023 includes), or with SSM Session Manager.

resource "aws_iam_role" "vm" {
  name = "${var.name}-vm"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

# What the cache needs, as ci-linux-role has in ci-gocache-aws (but without
# its bucket policy, which requires conditional writes), plus the
# DescribeTable and ListBucket permissions of awscache's Check.
resource "aws_iam_role_policy" "vm_cache" {
  name = "cache"
  role = aws_iam_role.vm.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "dynamodb:DescribeTable",
          "dynamodb:GetItem",
          "dynamodb:PutItem",
          "dynamodb:UpdateItem",
        ]
        Resource = aws_dynamodb_table.lab.arn
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${aws_s3_bucket.blobs.arn}/*"
      },
      {
        # Without ListBucket, a HEAD or GET of a missing object returns
        # 403 rather than 404.
        Effect   = "Allow"
        Action   = "s3:ListBucket"
        Resource = aws_s3_bucket.blobs.arn
      },
      {
        # Directory buckets authorize sessions, not operations. A
        # ReadWrite session (the default) allows every object operation,
        # including DeleteObject.
        Effect   = "Allow"
        Action   = "s3express:CreateSession"
        Resource = aws_s3_directory_bucket.express.arn
      },
    ]
  })
}

resource "aws_iam_role_policy_attachment" "vm_ssm" {
  role       = aws_iam_role.vm.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "vm" {
  name = "${var.name}-vm"
  role = aws_iam_role.vm.name
}

resource "aws_security_group" "vm" {
  name        = "${var.name}-vm"
  description = "${var.name} VM: SSH from var.my_ip only, all outbound"
  vpc_id      = aws_vpc.lab.id

  ingress {
    description = "SSH from var.my_ip"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["${var.my_ip}/32"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

resource "aws_instance" "vm" {
  ami                    = data.aws_ssm_parameter.al2023.value
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.lab.id
  vpc_security_group_ids = [aws_security_group.vm.id]
  iam_instance_profile   = aws_iam_instance_profile.vm.name

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.root_volume_gb
    iops        = 8000
    throughput  = 500
  }

  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    go_version    = var.go_version
    repo_url      = var.repo_url
    repo_branch   = var.repo_branch
    tailscale_ref = var.tailscale_ref
    test_env      = local.test_env
  })

  # A new AMI or setup script shouldn't replace the VM in the middle of an
  # experiment. (The setup script only runs at the first boot anyway.) To
  # apply them, replace the VM: terraform apply -replace=aws_instance.vm.
  lifecycle {
    ignore_changes = [ami, user_data]
  }

  tags = {
    Name = var.name
  }
}

locals {
  test_env = {
    AWS_REGION                   = var.region
    AWSCACHE_TEST_TABLE          = aws_dynamodb_table.lab.name
    AWSCACHE_TEST_BUCKET         = aws_s3_bucket.blobs.bucket
    AWSCACHE_TEST_EXPRESS_BUCKET = aws_s3_directory_bucket.express.bucket
  }
}

# A gocached server with its cache on instance-store NVMe, like today's
# ci-gocached-aws-1 (without S3 Files behind it), for the benchmark's
# gocached config, and in the role of an NVMe cache pod for its ddbgocached
# config. It has a fixed address, so that its URL doesn't change if it's
# replaced.

locals {
  gocached_ip  = cidrhost(aws_subnet.lab.cidr_block, 250)
  gocached_url = "http://${local.gocached_ip}:31364"
}

resource "aws_security_group" "gocached" {
  count = var.enable_gocached ? 1 : 0

  name        = "${var.name}-gocached"
  description = "${var.name} gocached VM: gocached from the benchmark VM, SSH from var.my_ip, all outbound"
  vpc_id      = aws_vpc.lab.id

  ingress {
    description     = "gocached (31364) and its debug server (31365) from the benchmark VM"
    from_port       = 31364
    to_port         = 31365
    protocol        = "tcp"
    security_groups = [aws_security_group.vm.id]
  }

  ingress {
    description = "SSH from var.my_ip"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["${var.my_ip}/32"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_instance" "gocached" {
  count = var.enable_gocached ? 1 : 0

  ami                    = data.aws_ssm_parameter.al2023.value
  instance_type          = var.gocached_instance_type
  subnet_id              = aws_subnet.lab.id
  private_ip             = local.gocached_ip
  vpc_security_group_ids = [aws_security_group.gocached[0].id]
  # For SSM Session Manager; gocached itself needs no AWS access.
  iam_instance_profile = aws_iam_instance_profile.vm.name

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = 30
  }

  user_data = templatefile("${path.module}/gocached-user-data.sh.tftpl", {
    go_version  = var.go_version
    repo_url    = var.repo_url
    repo_branch = var.repo_branch
    max_size_gb = var.gocached_max_size_gb
  })

  # As for the benchmark VM: to apply a new AMI or setup script, use
  # terraform apply -replace='aws_instance.gocached[0]'.
  lifecycle {
    ignore_changes = [ami, user_data]
  }

  tags = {
    Name = "${var.name}-gocached"
  }
}
