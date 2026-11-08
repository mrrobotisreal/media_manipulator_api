package burst

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// EC2API is the slice of *ec2.Client the provider uses, so tests capture the
// exact RunInstancesInput without any network.
type EC2API interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// EC2Provider is the on-demand AWS Provider. It never sets InstanceMarketOptions.
type EC2Provider struct {
	api EC2API
	log *slog.Logger
}

// NewEC2Provider wraps an EC2 API (a real client or a fake).
func NewEC2Provider(api EC2API, log *slog.Logger) *EC2Provider {
	if log == nil {
		log = slog.Default()
	}
	return &EC2Provider{api: api, log: log}
}

// NewEC2Client builds a client from the burst controller's static credentials,
// never the default chain, so the S3 client keeps its own credentials.
func NewEC2Client(ctx context.Context, region, accessKey, secretKey string) (*ec2.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithSharedConfigProfile(""),
	)
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(cfg), nil
}

// BuildRunInstancesInput is exported so the shape is testable and printable.
func BuildRunInstancesInput(req LaunchRequest, instanceType, subnet string) *ec2.RunInstancesInput {
	dev := req.RootDevice
	if dev == "" {
		dev = "/dev/xvda"
	}
	root := int32(req.RootGiB)
	if root <= 0 {
		root = 100
	}
	tags := []types.Tag{
		{Key: aws.String(TagBurst), Value: aws.String("1")},
		{Key: aws.String(TagRun), Value: aws.String(req.RunID)},
		{Key: aws.String(TagJob), Value: aws.String(req.JobID)},
		{Key: aws.String("Name"), Value: aws.String("mm-burst-" + req.Workload)},
	}
	in := &ec2.RunInstancesInput{
		ImageId:      aws.String(req.AMIFor(instanceType)),
		InstanceType: types.InstanceType(instanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		SubnetId:     aws.String(subnet),
		// On demand: InstanceMarketOptions is deliberately left nil.
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		SecurityGroupIds:                  []string{req.SecurityGroupID},
		IamInstanceProfile:                &types.IamInstanceProfileSpecification{Name: aws.String(req.InstanceProfile)},
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpTokens:              types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String(dev),
			Ebs: &types.EbsBlockDevice{
				VolumeType:          types.VolumeTypeGp3,
				VolumeSize:          aws.Int32(root),
				Encrypted:           aws.Bool(true),
				DeleteOnTermination: aws.Bool(true),
			},
		}},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
		// The run id makes a retried launch idempotent.
		ClientToken: aws.String(req.RunID),
	}
	if req.UserData != "" {
		in.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(req.UserData)))
	}
	return in
}

// MapEC2Error turns an AWS error into a typed one (the original is wrapped).
func MapEC2Error(err error) error {
	if err == nil {
		return nil
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "InsufficientInstanceCapacity", "Unsupported", "InsufficientFreeAddressesInSubnet", "SpotMaxPriceTooLow":
			return fmt.Errorf("%w: %s", ErrNoCapacity, ae.ErrorCode())
		case "VcpuLimitExceeded", "InstanceLimitExceeded", "MaxSpotInstanceCountExceeded":
			return fmt.Errorf("%w: %s", ErrQuota, ae.ErrorCode())
		case "UnauthorizedOperation", "AuthFailure", "OptInRequired":
			return fmt.Errorf("%w: %s", ErrDenied, ae.ErrorCode())
		}
	}
	return err
}

// Launch tries candidates x subnets, in order, stopping at the first success.
// ErrNoCapacity and ErrQuota move on to the next subnet/candidate; ErrDenied and
// any other error stop at once with no retry.
func (p *EC2Provider) Launch(ctx context.Context, req LaunchRequest) (Instance, error) {
	if req.RunID == "" || req.AMIFor == nil || len(req.Candidates) == 0 || len(req.Subnets) == 0 {
		return Instance{}, errors.New("burst: launch request needs run id, AMI, candidates and subnets")
	}
	var last error
	for _, typ := range req.Candidates {
		for _, subnet := range req.Subnets {
			if err := ctx.Err(); err != nil {
				return Instance{}, err
			}
			out, err := p.api.RunInstances(ctx, BuildRunInstancesInput(req, typ, subnet))
			if err == nil {
				if len(out.Instances) == 0 {
					return Instance{}, errors.New("burst: RunInstances returned no instance")
				}
				inst := toInstance(out.Instances[0])
				p.log.Info("burst: instance launched", "run", req.RunID, "instance", inst.ID, "type", typ, "subnet", subnet)
				return inst, nil
			}
			mapped := MapEC2Error(err)
			p.log.Warn("burst: launch attempt failed", "run", req.RunID, "type", typ, "subnet", subnet, "error", mapped.Error())
			last = mapped
			switch {
			case errors.Is(mapped, ErrDenied):
				return Instance{}, mapped
			case errors.Is(mapped, ErrNoCapacity), errors.Is(mapped, ErrQuota):
				continue
			default:
				return Instance{}, mapped
			}
		}
	}
	if last == nil {
		last = ErrNoCapacity
	}
	return Instance{}, last
}

func toInstance(i types.Instance) Instance {
	out := Instance{
		ID: aws.ToString(i.InstanceId), Type: string(i.InstanceType), AMI: aws.ToString(i.ImageId),
	}
	if i.State != nil {
		out.State = string(i.State.Name)
	}
	if i.Placement != nil {
		out.AZ = aws.ToString(i.Placement.AvailabilityZone)
	}
	if i.LaunchTime != nil {
		out.LaunchTime = *i.LaunchTime
	}
	for _, t := range i.Tags {
		switch aws.ToString(t.Key) {
		case TagRun:
			out.RunID = aws.ToString(t.Value)
		case TagJob:
			out.JobID = aws.ToString(t.Value)
		}
	}
	return out
}

func flatten(out *ec2.DescribeInstancesOutput) []Instance {
	var res []Instance
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			res = append(res, toInstance(i))
		}
	}
	return res
}

func (p *EC2Provider) Describe(ctx context.Context, ids []string) ([]Instance, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := p.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		return nil, MapEC2Error(err)
	}
	return flatten(out), nil
}

func (p *EC2Provider) ListTagged(ctx context.Context) ([]Instance, error) {
	in := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + TagBurst), Values: []string{"1"}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping"}},
	}}
	var res []Instance
	for {
		out, err := p.api.DescribeInstances(ctx, in)
		if err != nil {
			return nil, MapEC2Error(err)
		}
		res = append(res, flatten(out)...)
		if out.NextToken == nil || *out.NextToken == "" {
			return res, nil
		}
		in.NextToken = out.NextToken
	}
}

func (p *EC2Provider) Terminate(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids})
	if err != nil {
		// An already-gone instance is a success for a terminate.
		if strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
			return nil
		}
		return MapEC2Error(err)
	}
	p.log.Info("burst: instances terminated", "ids", strings.Join(ids, ","))
	return nil
}
