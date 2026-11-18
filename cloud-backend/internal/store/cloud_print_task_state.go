package store

import "errors"

// CloudPrintTaskState represents a stage of one document within a paid order.
// The ambiguous state RECONCILE is intentionally NOT automatically retryable.
type CloudPrintTaskState string

const (
 CloudTaskAwaitingPayment CloudPrintTaskState = "AWAITING_PAYMENT"
 CloudTaskReady CloudPrintTaskState = "READY"
 CloudTaskDispatching CloudPrintTaskState = "DISPATCHING"
 CloudTaskSubmitted CloudPrintTaskState = "SUBMITTED"
 CloudTaskCompleted CloudPrintTaskState = "COMPLETED"
 CloudTaskReconcile CloudPrintTaskState = "RECONCILE"
 CloudTaskCanceled CloudPrintTaskState = "CANCELED"
)

var ErrUnsafePrintTaskTransition = errors.New("unsafe cloud print task transition")

// A payment may release the first task only. Later tasks are released after
// the preceding one is durably marked COMPLETED. The caller must additionally
// validate payment, CUPS evidence and ownership in the eventual transaction.
//
// A crash during dispatch, a lost CUPS acknowledgement, unknown physical
// output or disputed page count MUST enter RECONCILE. Never automatically
// send the same paper twice based solely on an HTTP retry.
func ValidateCloudPrintTaskTransition(from,to CloudPrintTaskState,
 priorCompleted,paid,cupsEvidence,operatorReconciled bool) error {
 switch from {
 case CloudTaskAwaitingPayment:
  if to==CloudTaskReady && paid && priorCompleted{return nil}
  if to==CloudTaskCanceled && !paid{return nil}
 case CloudTaskReady:
  if to==CloudTaskDispatching && paid && priorCompleted{return nil}
  if to==CloudTaskCanceled && !paid{return nil}
 case CloudTaskDispatching:
  if to==CloudTaskSubmitted && cupsEvidence{return nil}
  if to==CloudTaskReconcile{return nil}
 case CloudTaskSubmitted:
  if to==CloudTaskCompleted && cupsEvidence{return nil}
  if to==CloudTaskReconcile{return nil}
 case CloudTaskReconcile:
  // Operator reconciliation requires independent physical/CUPS evidence.
  // A blind automated retry from this state is never allowed.
  if operatorReconciled && cupsEvidence && to==CloudTaskCompleted{return nil}
  if operatorReconciled && to==CloudTaskCanceled{return nil}
 case CloudTaskCompleted,CloudTaskCanceled:
  // Terminal states are immutable.
 }
 return ErrUnsafePrintTaskTransition
}
