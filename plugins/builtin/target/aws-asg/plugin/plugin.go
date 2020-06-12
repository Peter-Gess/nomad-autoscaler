package plugin

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-autoscaler/helper/nomad"
	"github.com/hashicorp/nomad-autoscaler/helper/scaleutils"
	"github.com/hashicorp/nomad-autoscaler/plugins"
	"github.com/hashicorp/nomad-autoscaler/plugins/base"
	"github.com/hashicorp/nomad-autoscaler/plugins/strategy"
	"github.com/hashicorp/nomad-autoscaler/plugins/target"
)

const (
	// pluginName is the unique name of the this plugin amongst Target plugins.
	pluginName = "aws-asg"

	// configKeys represents the known configuration parameters required at
	// varying points throughout the plugins lifecycle.
	configKeyRegion        = "region"
	configKeyAccessID      = "aws_access_key_id"
	configKeySecretKey     = "aws_secret_access_key"
	configKeySessionToken  = "session_token"
	configKeyASGName       = "asg_name"
	configKeyClass         = "class"
	configKeyDrainDeadline = "drain_deadline"

	// configValues are the default values used when a configuration key is not
	// supplied by the operator that are specific to the plugin.
	configValueRegionDefault = "us-east-1"
)

var (
	PluginConfig = &plugins.InternalPluginConfig{
		Factory: func(l hclog.Logger) interface{} { return NewAWSASGPlugin(l) },
	}

	pluginInfo = &base.PluginInfo{
		Name:       pluginName,
		PluginType: plugins.PluginTypeTarget,
	}
)

// Assert that TargetPlugin meets the target.Target interface.
var _ target.Target = (*TargetPlugin)(nil)

// TargetPlugin is the AWS ASG implementation of the target.Target interface.
type TargetPlugin struct {
	config       map[string]string
	logger       hclog.Logger
	asg          *autoscaling.Client
	ec2          *ec2.Client
	scaleInUtils *scaleutils.ScaleIn
}

// NewAWSASGPlugin returns the AWS ASG implementation of the target.Target
// interface.
func NewAWSASGPlugin(log hclog.Logger) *TargetPlugin {
	return &TargetPlugin{
		logger: log,
	}
}

// SetConfig satisfies the SetConfig function on the base.Plugin interface.
func (t *TargetPlugin) SetConfig(config map[string]string) error {

	t.config = config

	if err := t.setupAWSClients(config); err != nil {
		return err
	}

	utils, err := scaleutils.NewScaleInUtils(nomad.ConfigFromMap(config), t.logger)
	if err != nil {
		return err
	}
	t.scaleInUtils = utils

	return nil
}

// PluginInfo satisfies the PluginInfo function on the base.Plugin interface.
func (t *TargetPlugin) PluginInfo() (*base.PluginInfo, error) {
	return pluginInfo, nil
}

// Scale satisfies the Scale function on the target.Target interface.
func (t *TargetPlugin) Scale(action strategy.Action, config map[string]string) error {

	// We cannot scale an ASG without knowing the ASG name.
	asgName, ok := config[configKeyASGName]
	if !ok {
		return fmt.Errorf("required config param %s not found", configKeyASGName)
	}
	ctx := context.Background()

	// Describe the ASG. This serves to both validate the config value is
	// correct and ensure the AWS client is configured correctly. The response
	// can also be used when performing the scaling, meaning we only need to
	// call it once.
	curASG, err := t.describeASG(ctx, asgName)
	if err != nil {
		return fmt.Errorf("failed to describe AWS Autoscaling Group: %v", err)
	}

	// The AWS ASG target requires different details depending on which
	// direction we want to scale. Therefore calculate the direction and the
	// relevant number so we can correctly perform the AWS work.
	num, direction := t.calculateDirection(*curASG.DesiredCapacity, action.Count)

	switch direction {
	case "in":
		err = t.scaleIn(ctx, curASG, num, config)
	case "out":
		err = t.scaleOut(ctx, curASG, num)
	default:
		return fmt.Errorf("scaling not required, ASG count %v and Autoscaler desired count %v",
			*curASG.DesiredCapacity, action.Count)
	}

	// If we received an error while scaling, format this with an outer message
	// so its nice for the operators and then return any error to the caller.
	if err != nil {
		err = fmt.Errorf("failed to perform scaling action: %v", err)
	}
	return err
}

// Status satisfies the Status function on the target.Target interface.
func (t *TargetPlugin) Status(config map[string]string) (*target.Status, error) {

	// We cannot get the status of an ASG if we don't know its name.
	asgName, ok := config[configKeyASGName]
	if !ok {
		return nil, fmt.Errorf("required config param %s not found", configKeyASGName)
	}
	ctx := context.Background()

	asg, err := t.describeASG(ctx, asgName)
	if err != nil {
		return nil, fmt.Errorf("failed to describe AWS Autoscaling Group: %v", err)
	}

	events, err := t.describeActivities(ctx, asgName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to describe AWS Autoscaling Group activities: %v", err)
	}

	resp := target.Status{
		Ready: asg.Status == nil,
		Count: *asg.DesiredCapacity,
	}

	// If the ASG has scaling activities listed ensure the status takes into
	// account the most recent activity. Most importantly if the last event has
	// not finished, the ASG is not ready for scaling.
	if events != nil {
		resp.Ready = resp.Ready || *events[0].Progress == 100
		resp.Meta = map[string]string{
			target.MetaKeyLastEvent: strconv.FormatInt(events[0].EndTime.UnixNano(), 10),
		}
	}

	return &resp, nil
}

func (t *TargetPlugin) calculateDirection(asgDesired, strategyDesired int64) (int64, string) {

	if strategyDesired < asgDesired {
		return asgDesired - strategyDesired, "in"
	}
	if strategyDesired > asgDesired {
		return strategyDesired, "out"
	}
	return 0, ""
}
