package upgrade

import (
	"context"
	"reflect"
	"strings"

	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/global"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/jaegertracing/jaeger-operator/pkg/apis/jaegertracing/v1"
	"github.com/jaegertracing/jaeger-operator/pkg/tracing"
)

// ManagedInstances finds all the Jaeger instances for the current operator and upgrades them, if necessary
func ManagedInstances(ctx context.Context, c client.Client, reader client.Reader) error {
	tracer := global.TraceProvider().GetTracer(v1.ReconciliationTracer)
	ctx, span := tracer.Start(ctx, "ManagedInstances")
	defer span.End()

	list := &v1.JaegerList{}
	identity := viper.GetString(v1.ConfigIdentity)
	opts := []client.ListOption{}
	opts = append(opts, client.MatchingLabels(map[string]string{
		v1.LabelOperatedBy: identity,
	}))

	// if set and np cluster scope permission, skip
	// if not set, treat as true
	if (viper.IsSet("has-cluster-permission") && viper.GetBool("has-cluster-permission")) || !viper.IsSet("has-cluster-permission") {
		if err := reader.List(ctx, list, opts...); err != nil {
			if strings.HasSuffix(err.Error(), "cluster scope") {
				log.WithError(err).Warn("List indentity failed with cluster scope")
				watchNs, e := k8sutil.GetWatchNamespace()
				if e != nil {
					return tracing.HandleError(e, span)
				}
				// retry with watchnamespace
				opts = append(opts, client.InNamespace(watchNs))
				log.WithFields(log.Fields{
					"namespace": watchNs,
				}).Info("retry with namespaced scope")
				if e := reader.List(ctx, list, opts...); e != nil {
					return tracing.HandleError(e, span)
				}
			} else {
				return tracing.HandleError(err, span)
			}
		}
	}

	for _, j := range list.Items {
		// this check shouldn't have been necessary, as I'd expect the list of items to come filtered out already
		// but apparently, at least the fake client used in the unit tests doesn't filter it out... so, let's double-check
		// that we indeed own the item
		owner := j.Labels[v1.LabelOperatedBy]
		if owner != identity {
			log.WithFields(log.Fields{
				"our-identity":   identity,
				"owner-identity": owner,
			}).Debug("skipping CR upgrade as we are not owners")
			continue
		}

		jaeger, err := ManagedInstance(ctx, c, j)
		if err != nil {
			// nothing to do at this level, just go to the next instance
			continue
		}

		if !reflect.DeepEqual(jaeger, j) {
			// the CR has changed, store it!
			if err := c.Update(ctx, &jaeger); err != nil {
				log.WithFields(log.Fields{
					"instance":  jaeger.Name,
					"namespace": jaeger.Namespace,
				}).WithError(err).Error("failed to store the upgraded instance")
				tracing.HandleError(err, span)
			}
		}
	}

	return nil
}

// ManagedInstance performs the necessary changes to bring the given Jaeger instance to the current version
func ManagedInstance(ctx context.Context, client client.Client, jaeger v1.Jaeger) (v1.Jaeger, error) {
	tracer := global.TraceProvider().GetTracer(v1.ReconciliationTracer)
	ctx, span := tracer.Start(ctx, "ManagedInstance")
	defer span.End()

	if v, ok := versions[jaeger.Status.Version]; ok {
		// we don't need to run the upgrade function for the version 'v', only the next ones
		for n := v.next; n != nil; n = n.next {
			// performs the upgrade to version 'n'
			upgraded, err := n.upgrade(ctx, client, jaeger)
			if err != nil {
				log.WithFields(log.Fields{
					"instance":  jaeger.Name,
					"namespace": jaeger.Namespace,
					"to":        n.v,
				}).WithError(err).Warn("failed to upgrade managed instance")
				return jaeger, tracing.HandleError(err, span)
			}

			upgraded.Status.Version = n.v
			jaeger = upgraded
		}
	}

	return jaeger, nil
}
