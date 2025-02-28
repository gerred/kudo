package health

import (
	"context"
	"fmt"
	"log"

	kudov1alpha1 "github.com/kudobuilder/kudo/pkg/apis/kudo/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsHealthy returns whether an object is healthy. Must be implemented for each type.
func IsHealthy(c client.Client, obj runtime.Object) error {

	switch obj := obj.(type) {
	case *appsv1.StatefulSet:
		if obj.Spec.Replicas == nil {
			return fmt.Errorf("replicas not set, so can't be healthy")
		}
		if obj.Status.ReadyReplicas == *obj.Spec.Replicas {
			log.Printf("Statefulset %v is marked healthy\n", obj.Name)
			return nil
		}
		log.Printf("HealthUtil: Statefulset %v is NOT healthy. Not enough ready replicas: %v/%v", obj.Name, obj.Status.ReadyReplicas, obj.Status.Replicas)
		return fmt.Errorf("ready replicas (%v) does not equal requested replicas (%v)", obj.Status.ReadyReplicas, obj.Status.Replicas)
	case *appsv1.Deployment:
		if obj.Spec.Replicas != nil && obj.Status.ReadyReplicas == *obj.Spec.Replicas {
			log.Printf("HealthUtil: Deployment %v is marked healthy", obj.Name)
			return nil
		}
		log.Printf("HealthUtil: Deployment %v is NOT healthy. Not enough ready replicas: %v/%v", obj.Name, obj.Status.ReadyReplicas, *obj.Spec.Replicas)
		return fmt.Errorf("ready replicas (%v) does not equal requested replicas (%v)", obj.Status.ReadyReplicas, *obj.Spec.Replicas)
	case *batchv1.Job:

		if obj.Status.Succeeded == int32(1) {
			// Done!
			log.Printf("HealthUtil: Job \"%v\" is marked healthy", obj.Name)
			return nil
		}
		return fmt.Errorf("job \"%v\" still running or failed", obj.Name)
	case *kudov1alpha1.Instance:
		// Instances are healthy when their Active Plan has succeeded
		plan := &kudov1alpha1.PlanExecution{}
		if obj.Status.ActivePlan.Name == "" {
			return fmt.Errorf("checking health of instance %s: not healthy because does not have any active plan assigned yet", obj.Name)
		}
		err := c.Get(context.TODO(), client.ObjectKey{
			Name:      obj.Status.ActivePlan.Name,
			Namespace: obj.Status.ActivePlan.Namespace,
		}, plan)
		if err != nil {
			log.Printf("Error getting PlaneExecution %v/%v: %v\n", obj.Status.ActivePlan.Name, obj.Status.ActivePlan.Namespace, err)
			return fmt.Errorf("instance active plan not found: %v", err)
		}
		log.Printf("HealthUtil: Instance %v is in state %v", obj.Name, plan.Status.State)

		if plan.Status.State == kudov1alpha1.PhaseStateComplete {
			return nil
		}
		return fmt.Errorf("instance's active plan is in state %v", plan.Status.State)

	// unless we build logic for what a healthy object is, assume it's healthy when created.
	default:
		log.Printf("HealthUtil: Unknown type is marked healthy by default")
		return nil
	}
}
