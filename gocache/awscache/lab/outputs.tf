output "instance_id" {
  description = "The lab VM."
  value       = aws_instance.vm.id
}

output "ssh_command" {
  description = "Opens a shell on the lab VM as ec2-user, with EC2 Instance Connect, from var.my_ip."
  value       = "aws ec2-instance-connect ssh --region ${var.region} --instance-id ${aws_instance.vm.id} --os-user ec2-user --connection-type direct"
}

output "public_ip" {
  description = "The lab VM's public IP address."
  value       = aws_instance.vm.public_ip
}

output "ssm_command" {
  description = "Opens a shell on the lab VM. Then run `sudo -iu ec2-user`."
  value       = "aws ssm start-session --region ${var.region} --target ${aws_instance.vm.id}"
}

output "test_env" {
  description = "The environment for the tests. It's already set on the VM; use it to run the tests from elsewhere."
  value       = join("\n", [for k, v in local.test_env : "export ${k}=${v}"])
}

output "bucket" {
  description = "The S3 Standard bucket. The benchmark's README uses it to copy results to your laptop."
  value       = aws_s3_bucket.blobs.bucket
}

output "region" {
  description = "The lab's region, for AWS CLI commands."
  value       = var.region
}

output "gocached_url" {
  description = "gocached's URL, for the benchmark's -buildbench.gocached (or $AWSCACHE_BENCH_GOCACHED_URL on the benchmark VM)."
  value       = var.enable_gocached ? local.gocached_url : null
}

output "gocached_instance_id" {
  description = "The gocached VM."
  value       = var.enable_gocached ? aws_instance.gocached[0].id : null
}

output "gocached_ssh_command" {
  description = "Opens a shell on the gocached VM as ec2-user, with EC2 Instance Connect, from var.my_ip."
  value       = var.enable_gocached ? "aws ec2-instance-connect ssh --region ${var.region} --instance-id ${aws_instance.gocached[0].id} --os-user ec2-user --connection-type direct" : null
}
