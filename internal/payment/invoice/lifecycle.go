package invoice

import (
	"context"
	"errors"
	"fmt"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
)

// ErrEInvoiceProvider marks a configured e-invoice adapter's failure.
// The handler maps it to EINVOICE_PROVIDER_ERROR (502); the invoice row
// is left exactly as it was, so the operator can retry.
var ErrEInvoiceProvider = errors.New("e-invoice provider error")

// Issue moves one pending invoice to issued. It is the manual seam of
// spec §3.8: with no vendor adapter configured (Service.EInvoice nil) an
// operator's action is the whole transition. With an adapter configured
// the adapter runs first, its handle is stored, and only then does the
// row become issued.
//
// Only pending_issue can be issued: an optionally-issued invoice answers
// store.ErrInvoiceAlreadyIssued and a missing one store.ErrInvoiceNotFound.
// actorID is the operator behind the action and is recorded on the audit
// entry ("" for a system action).
func (s *Service) Issue(ctx context.Context, id, actorID string) (*model.Invoice, error) {
	inv, order, err := s.Store.GetInvoiceWithOrder(ctx, id)
	if err != nil {
		return nil, err
	}
	if inv.Status != model.InvoiceStatusPendingIssue {
		return nil, store.ErrInvoiceAlreadyIssued
	}

	ref := store.EInvoiceRef{}
	if s.EInvoice != nil {
		adapterRef, err := s.EInvoice.Issue(ctx, inv)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrEInvoiceProvider, err)
		}
		ref = store.EInvoiceRef{Provider: adapterRef.Provider, Ref: adapterRef.Ref, Status: adapterRef.Status}
		if ref.Provider == "" {
			ref.Provider = inv.EInvoiceProvider
		}
	}

	issued, err := s.Store.MarkInvoiceIssued(ctx, id, s.Now().UTC(), ref)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "invoice_issued", actorID, issued, map[string]any{
		"number": issued.Number, "e_invoice_provider": issued.EInvoiceProvider,
	})
	s.emitIssued(ctx, order.ProductID, issued)
	return issued, nil
}

// Void voids one invoice that was issued or is still pending. It is not
// a delete: the number, amounts and stored PDF stay, because the voided
// document is what an auditor needs to see. A void invoice cannot be
// voided again (store.ErrInvoiceNotVoidable).
func (s *Service) Void(ctx context.Context, id, actorID string) (*model.Invoice, error) {
	return s.VoidWithReason(ctx, id, actorID, "")
}

// VoidWithReason is Void plus the operator's note, recorded on the audit
// entry. The admin route uses it; the reason is optional.
func (s *Service) VoidWithReason(ctx context.Context, id, actorID, reason string) (*model.Invoice, error) {
	inv, order, err := s.Store.GetInvoiceWithOrder(ctx, id)
	if err != nil {
		return nil, err
	}
	if inv.Status == model.InvoiceStatusVoid {
		return nil, store.ErrInvoiceNotVoidable
	}
	// Only an e-invoice actually issued at a vendor can be voided there;
	// a manually issued invoice has no external ref.
	if s.EInvoice != nil && inv.EInvoiceRef != "" {
		if err := s.EInvoice.Void(ctx, EInvoiceRef{
			Provider: inv.EInvoiceProvider, Ref: inv.EInvoiceRef, Status: inv.EInvoiceStatus,
		}); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrEInvoiceProvider, err)
		}
	}

	voided, err := s.Store.MarkInvoiceVoid(ctx, id)
	if err != nil {
		return nil, err
	}
	changes := map[string]any{"number": voided.Number}
	if reason != "" {
		changes["reason"] = reason
	}
	s.audit(ctx, "invoice_voided", actorID, voided, changes)
	s.emitVoided(ctx, order.ProductID, voided)
	return voided, nil
}

// audit records an operator transition. It is best-effort like every
// other audit write: the state change is committed, and a lost audit
// line must not undo it.
func (s *Service) audit(ctx context.Context, action, actorID string, inv *model.Invoice, changes map[string]any) {
	if s.Store == nil || inv == nil {
		return
	}
	s.Store.Audit(ctx, &model.AuditLog{
		Entity: "invoice", EntityID: inv.ID, Action: action,
		ActorType: "admin", ActorID: actorID, Changes: changes,
	})
}
