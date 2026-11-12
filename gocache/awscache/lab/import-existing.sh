#!/bin/sh
# import-existing.sh finds the lab's resources in AWS and prints Terraform
# import blocks for them, for when the lab exists but its Terraform state
# is lost. Usage, from this directory:
#
#	aws-vault exec sandbox -- ./import-existing.sh [region] [name] > imports.tf
#	aws-vault exec sandbox --no-session -- terraform plan -var my_ip=...
#
# region defaults to $AWS_REGION, then us-east-1, and name (var.name) to
# awscache-lab. Resources it can't find are left out, and Terraform creates
# them. Check the plan, apply it, and then delete imports.tf.
set -eu

region=${1:-${AWS_REGION:-us-east-1}}
name=${2:-awscache-lab}

aws() { command aws --region "$region" --output text "$@"; }

# found prints its argument if it names something, and fails otherwise.
found() {
	case "$1" in
	"" | None | null) return 1 ;;
	esac
	printf '%s' "$1"
}

# imp prints an import block for address $1 and ID $2, or a comment if $2 is
# empty.
imp() {
	if [ -n "$2" ]; then
		printf 'import {\n  to = %s\n  id = "%s"\n}\n\n' "$1" "$2"
	else
		printf '# Not found, so Terraform will create it: %s\n\n' "$1"
	fi
}

account=$(aws sts get-caller-identity --query Account)

vpc=$(found "$(aws ec2 describe-vpcs --filters "Name=tag:Name,Values=$name" --query 'Vpcs[0].VpcId')") || vpc=
imp aws_vpc.lab "$vpc"

subnet= igw= rtb= sg_vm= sg_gocached=
if [ -n "$vpc" ]; then
	subnet=$(found "$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc" "Name=tag:Name,Values=$name" --query 'Subnets[0].SubnetId')") || subnet=
	igw=$(found "$(aws ec2 describe-internet-gateways --filters "Name=attachment.vpc-id,Values=$vpc" --query 'InternetGateways[0].InternetGatewayId')") || igw=
	rtb=$(found "$(aws ec2 describe-route-tables --filters "Name=vpc-id,Values=$vpc" "Name=tag:Name,Values=$name" --query 'RouteTables[0].RouteTableId')") || rtb=
	sg_vm=$(found "$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$vpc" "Name=group-name,Values=$name-vm" --query 'SecurityGroups[0].GroupId')") || sg_vm=
	sg_gocached=$(found "$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$vpc" "Name=group-name,Values=$name-gocached" --query 'SecurityGroups[0].GroupId')") || sg_gocached=
fi
imp aws_subnet.lab "$subnet"
imp aws_internet_gateway.lab "$igw"
imp aws_route_table.lab "$rtb"
if [ -n "$subnet" ] && [ -n "$rtb" ]; then
	imp aws_route_table_association.lab "$subnet/$rtb"
else
	imp aws_route_table_association.lab ""
fi

for svc in s3 s3express dynamodb; do
	ep=
	if [ -n "$vpc" ]; then
		ep=$(found "$(aws ec2 describe-vpc-endpoints --filters "Name=vpc-id,Values=$vpc" "Name=service-name,Values=com.amazonaws.$region.$svc" --query 'VpcEndpoints[0].VpcEndpointId')") || ep=
	fi
	imp "aws_vpc_endpoint.gateway[\"$svc\"]" "$ep"
done

table=$(found "$(aws dynamodb describe-table --table-name "$name" --query 'Table.TableName' 2>/dev/null)") || table=
imp aws_dynamodb_table.lab "$table"

bucket=$(found "$(aws s3api head-bucket --bucket "$name-$account" >/dev/null 2>&1 && echo "$name-$account")") || bucket=
imp aws_s3_bucket.blobs "$bucket"
imp aws_s3_bucket_public_access_block.blobs "$bucket"
lifecycle=
if [ -n "$bucket" ] && aws s3api get-bucket-lifecycle-configuration --bucket "$bucket" >/dev/null 2>&1; then
	lifecycle=$bucket
fi
imp aws_s3_bucket_lifecycle_configuration.blobs "$lifecycle"

express=$(found "$(aws s3api list-directory-buckets --query "Buckets[?starts_with(Name, '$name-$account--')].Name | [0]")") || express=
imp aws_s3_directory_bucket.express "$express"

role=$(found "$(aws iam get-role --role-name "$name-vm" --query 'Role.RoleName' 2>/dev/null)") || role=
imp aws_iam_role.vm "$role"
policy=
if [ -n "$role" ] && aws iam get-role-policy --role-name "$role" --policy-name cache >/dev/null 2>&1; then
	policy=$role:cache
fi
imp aws_iam_role_policy.vm_cache "$policy"
ssm=arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
attachment=
if [ -n "$role" ] && aws iam list-attached-role-policies --role-name "$role" --query 'AttachedPolicies[].PolicyArn' | grep -q "$ssm"; then
	attachment=$role/$ssm
fi
imp aws_iam_role_policy_attachment.vm_ssm "$attachment"
profile=$(found "$(aws iam get-instance-profile --instance-profile-name "$name-vm" --query 'InstanceProfile.InstanceProfileName' 2>/dev/null)") || profile=
imp aws_iam_instance_profile.vm "$profile"

imp aws_security_group.vm "$sg_vm"

instance() {
	found "$(aws ec2 describe-instances --filters "Name=tag:Name,Values=$1" "Name=instance-state-name,Values=pending,running,stopping,stopped" --query 'Reservations[0].Instances[0].InstanceId')" || true
}
imp aws_instance.vm "$(instance "$name")"

# The gocached VM, if it was created. Its resources are indexed (count).
gocached=$(instance "$name-gocached")
if [ -n "$sg_gocached" ] || [ -n "$gocached" ]; then
	imp 'aws_security_group.gocached[0]' "$sg_gocached"
	imp 'aws_instance.gocached[0]' "$gocached"
fi
