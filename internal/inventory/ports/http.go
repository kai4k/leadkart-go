package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/inventory/app"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// AddRoutes registers Inventory HTTP handlers on mux. Mat Ryer 2024
// canon: ports own request/response translation, not the routing
// scheme — composition root chooses the URL space.
//
// verifier + stampValidator gate authenticated routes. When BOTH are
// non-nil the inventory route block registers; (nil, nil) keeps the
// surface unregistered (test fixtures that only exercise unauthenticated
// surface pass nil/nil).
//
// All routes under /api/v1/inventory/* run under TxScopeTenant via the
// JWT-bridge middleware — `app.tenant_id` GUC bound to caller's
// tenant; Postgres RLS does the rest.
//
//nolint:funlen // route table — single-source-of-truth registration list
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier, stampValidator authn.StampValidator) {
	if verifier == nil || stampValidator == nil {
		return
	}

	// Per-route permission gates. Catalog gates ride
	// inventory.catalog.{read,manage}; Stock gates ride
	// inventory.stock.{read,manage}. RequirePermission +
	// RequireAnyPermission already compose RequireFreshStamp internally
	// (per Wave 9.2 + ADR 0036) — JWT signature + expiry +
	// security_stamp freshness all run BEFORE the permission check.
	catalogRead := authn.RequireAnyPermission(verifier, stampValidator,
		permission.IdentityPermissions.Inventory.Catalog.Read,
		permission.IdentityPermissions.Inventory.Catalog.Manage)
	catalogManage := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.Inventory.Catalog.Manage)
	stockRead := authn.RequireAnyPermission(verifier, stampValidator,
		permission.IdentityPermissions.Inventory.Stock.Read,
		permission.IdentityPermissions.Inventory.Stock.Manage)
	stockManage := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.Inventory.Stock.Manage)

	// ----- Products -----------------------------------------------------------
	mux.Handle("POST /api/v1/inventory/products",
		catalogManage(handleCreateProduct(log, a)))
	mux.Handle("GET /api/v1/inventory/products",
		catalogRead(handleListProducts(log, a)))
	mux.Handle("GET /api/v1/inventory/products/{productId}",
		catalogRead(handleGetProduct(log, a)))
	mux.Handle("PATCH /api/v1/inventory/products/{productId}",
		catalogManage(handleUpdateProduct(log, a)))
	mux.Handle("DELETE /api/v1/inventory/products/{productId}",
		catalogManage(handleDeleteProduct(log, a)))

	// ----- Batches ------------------------------------------------------------
	mux.Handle("POST /api/v1/inventory/products/{productId}/batches",
		stockManage(handleAddBatch(log, a)))
	mux.Handle("GET /api/v1/inventory/products/{productId}/batches",
		stockRead(handleListBatchesForProduct(log, a)))
	mux.Handle("GET /api/v1/inventory/batches/{batchId}",
		stockRead(handleGetBatch(log, a)))

	// ----- Stock movements ---------------------------------------------------
	mux.Handle("POST /api/v1/inventory/batches/{batchId}/movements",
		stockManage(handleLogStockMovement(log, a)))
	mux.Handle("GET /api/v1/inventory/batches/{batchId}/movements",
		stockRead(handleListBatchMovements(log, a)))
}

// ----- Handlers --------------------------------------------------------------

func handleCreateProduct(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		var req CreateProductRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		out, err := a.Commands.CreateProduct.Handle(r.Context(), command.CreateProductCommand{
			TenantID:          tenant.ID(c.TenantID),
			ActorMembershipID: membership.ID(c.MembershipID),
			SKU:               req.SKU,
			Name:              req.Name,
			DosageForm:        req.DosageForm,
			PackSize:          req.PackSize,
			HSNCode:           req.HSNCode,
			GSTRateBps:        req.GSTRateBps,
			Manufacturer:      req.Manufacturer,
		})
		switch {
		case errors.Is(err, product.ErrSKUTaken):
			writeError(w, http.StatusConflict, ErrCodeAlreadyExists, "sku already exists in this tenant")
			return
		case errors.Is(err, product.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "create product failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, CreateProductResponse{ProductID: out.ProductID.String()})
	})
}

func handleListProducts(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		q := r.URL.Query()
		cursor, err := pagination.Decode(q.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidQuery, "invalid cursor")
			return
		}
		pageSize := parsePageSize(q.Get("page_size"))
		activeFilter := q.Get("active")
		page, err := a.Queries.ListProductsPage.Handle(r.Context(), query.ListProductsPageQuery{
			TenantID:     tenant.ID(c.TenantID),
			Cursor:       cursor,
			PageSize:     pageSize,
			ActiveOnly:   activeFilter == "true",
			DosageForm:   strings.TrimSpace(q.Get("dosage_form")),
			Manufacturer: strings.TrimSpace(q.Get("manufacturer")),
			Search:       strings.TrimSpace(q.Get("search")),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list products failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		items := make([]ProductDto, 0, len(page.Items))
		for _, p := range page.Items {
			items = append(items, productToDto(p))
		}
		writeJSON(w, http.StatusOK, ListProductsResponse{
			Items:      items,
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		})
	})
}

func handleGetProduct(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		id, ok := parsePathUUID(w, r, "productId")
		if !ok {
			return
		}
		p, err := a.Queries.GetProduct.Handle(r.Context(), query.GetProductQuery{
			TenantID:  tenant.ID(c.TenantID),
			ProductID: product.ID(id.String()),
		})
		switch {
		case errors.Is(err, product.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get product failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, productToDto(p))
	})
}

func handleUpdateProduct(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		id, ok := parsePathUUID(w, r, "productId")
		if !ok {
			return
		}
		var req UpdateProductRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.UpdateProduct.Handle(r.Context(), command.UpdateProductCommand{
			TenantID:          tenant.ID(c.TenantID),
			ProductID:         product.ID(id.String()),
			ActorMembershipID: membership.ID(c.MembershipID),
			Name:              req.Name,
			GSTRateBps:        req.GSTRateBps,
			IsActive:          req.IsActive,
			Manufacturer:      req.Manufacturer,
		})
		switch {
		case errors.Is(err, product.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "")
			return
		case errors.Is(err, product.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, err.Error())
			return
		case errors.Is(err, product.ErrDeleted):
			writeError(w, http.StatusConflict, ErrCodeConflict, "product is deleted")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "update product failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func handleDeleteProduct(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		id, ok := parsePathUUID(w, r, "productId")
		if !ok {
			return
		}
		err := a.Commands.DeleteProduct.Handle(r.Context(), command.DeleteProductCommand{
			TenantID:          tenant.ID(c.TenantID),
			ProductID:         product.ID(id.String()),
			ActorMembershipID: membership.ID(c.MembershipID),
		})
		switch {
		case errors.Is(err, product.ErrNotFound):
			// Soft-delete is idempotent: missing OR already-deleted → 204
			// (mirrors Stripe DELETE semantics).
			w.WriteHeader(http.StatusNoContent)
			return
		case errors.Is(err, batch.ErrAnyLiveStock):
			writeError(w, http.StatusConflict, ErrCodeProductHasStock,
				"cannot delete product with live batches holding stock")
			return
		case errors.Is(err, product.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "delete product failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func handleAddBatch(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		productID, ok := parsePathUUID(w, r, "productId")
		if !ok {
			return
		}
		var req AddBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		out, err := a.Commands.AddBatch.Handle(r.Context(), command.AddBatchCommand{
			TenantID:                   tenant.ID(c.TenantID),
			ProductID:                  product.ID(productID.String()),
			ActorMembershipID:          membership.ID(c.MembershipID),
			BatchNumber:                req.BatchNumber,
			ManufactureDate:            req.ManufactureDate,
			ExpiryDate:                 req.ExpiryDate,
			ManufacturerName:           req.ManufacturerName,
			ManufacturingLicenceNumber: req.ManufacturingLicenceNumber,
			MRPPaise:                   req.MRPPaise,
			PurchasePricePaise:         req.PurchasePricePaise,
		})
		switch {
		case errors.Is(err, product.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "product not found")
			return
		case errors.Is(err, batch.ErrBatchNumberTaken):
			writeError(w, http.StatusConflict, ErrCodeAlreadyExists, "batch_number already exists for this product")
			return
		case errors.Is(err, batch.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "add batch failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, AddBatchResponse{BatchID: out.BatchID.String()})
	})
}

func handleListBatchesForProduct(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		productID, ok := parsePathUUID(w, r, "productId")
		if !ok {
			return
		}
		q := r.URL.Query()
		cursor, err := pagination.Decode(q.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidQuery, "invalid cursor")
			return
		}
		page, err := a.Queries.ListBatchesByProduct.Handle(r.Context(), query.ListBatchesByProductQuery{
			TenantID:       tenant.ID(c.TenantID),
			ProductID:      product.ID(productID.String()),
			Cursor:         cursor,
			PageSize:       parsePageSize(q.Get("page_size")),
			IncludeExpired: q.Get("include_expired") == "true",
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list batches failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		items := make([]BatchDto, 0, len(page.Items))
		for _, b := range page.Items {
			items = append(items, batchToDto(b))
		}
		writeJSON(w, http.StatusOK, ListBatchesResponse{
			Items:      items,
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		})
	})
}

func handleGetBatch(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		id, ok := parsePathUUID(w, r, "batchId")
		if !ok {
			return
		}
		b, err := a.Queries.GetBatch.Handle(r.Context(), query.GetBatchQuery{
			TenantID: tenant.ID(c.TenantID),
			BatchID:  batch.ID(id.String()),
		})
		switch {
		case errors.Is(err, batch.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get batch failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, batchToDto(b))
	})
}

func handleLogStockMovement(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		batchID, ok := parsePathUUID(w, r, "batchId")
		if !ok {
			return
		}
		var req LogMovementRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		out, err := a.Commands.LogStockMovement.Handle(r.Context(), command.LogStockMovementCommand{
			TenantID:          tenant.ID(c.TenantID),
			BatchID:           batch.ID(batchID.String()),
			ActorMembershipID: membership.ID(c.MembershipID),
			Type:              batch.MovementType(req.Type),
			Quantity:          req.Quantity,
			Reason:            req.Reason,
			SourceReference:   req.SourceReference,
		})
		switch {
		case errors.Is(err, batch.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "batch not found")
			return
		case errors.Is(err, batch.ErrInsufficientStock):
			writeError(w, http.StatusConflict, ErrCodeInsufficientStock, err.Error())
			return
		case errors.Is(err, batch.ErrExpired):
			writeError(w, http.StatusConflict, ErrCodeBatchExpired, err.Error())
			return
		case errors.Is(err, batch.ErrDeleted):
			writeError(w, http.StatusConflict, ErrCodeConflict, "batch is deleted")
			return
		case errors.Is(err, batch.ErrConcurrencyConflict):
			writeError(w, http.StatusConflict, ErrCodeConcurrencyConflict,
				"batch was modified concurrently; retry")
			return
		case errors.Is(err, batch.ErrInvalid),
			errors.Is(err, stockmovement.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "log stock movement failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, LogMovementResponse{
			MovementID:          out.MovementID.String(),
			QuantityOnHandAfter: out.QuantityOnHandAfter,
		})
	})
}

func handleListBatchMovements(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		batchID, ok := parsePathUUID(w, r, "batchId")
		if !ok {
			return
		}
		q := r.URL.Query()
		cursor, err := pagination.Decode(q.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidQuery, "invalid cursor")
			return
		}
		var filterType batch.MovementType
		if t := strings.TrimSpace(q.Get("type")); t != "" {
			filterType = batch.MovementType(t)
		}
		page, err := a.Queries.ListBatchMovementsPage.Handle(r.Context(), query.ListBatchMovementsPageQuery{
			TenantID: tenant.ID(c.TenantID),
			BatchID:  batch.ID(batchID.String()),
			Cursor:   cursor,
			PageSize: parsePageSize(q.Get("page_size")),
			Type:     filterType,
		})
		switch {
		case errors.Is(err, stockmovement.ErrInvalid):
			writeError(w, http.StatusBadRequest, ErrCodeInvalidQuery, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "list movements failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		items := make([]MovementDto, 0, len(page.Items))
		for _, m := range page.Items {
			items = append(items, movementToDto(m))
		}
		writeJSON(w, http.StatusOK, ListMovementsResponse{
			Items:      items,
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		})
	})
}

// ----- Helpers --------------------------------------------------------------

// parsePathUUID extracts + validates a UUID path value; writes 400 +
// returns (uuid.Nil, false) on bad input.
func parsePathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidID,
			name+" path parameter must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

// parsePageSize returns the supplied page_size as int, defaulting to 0
// (the query handler then runs pagination.ClampPageSize).
func parsePageSize(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// writeJSON encodes body to w with status code.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits the canonical RFC 9457 Problem Details + legacy
// `error` / `message` shape per the rest of the LeadKart API.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		Type:    problemType(code),
		Title:   http.StatusText(status),
		Status:  status,
		Detail:  message,
		Error:   code,
		Message: message,
	})
}

// problemType returns the RFC 9457 `type` URI for an error code.
func problemType(code string) string {
	if code == "" {
		return ""
	}
	return "https://leadkart.api/errors/" + code
}

// ----- Projections ----------------------------------------------------------

func productToDto(p *product.Product) ProductDto {
	return ProductDto{
		ID:           p.ID().String(),
		TenantID:     p.TenantID().String(),
		SKU:          p.SKU(),
		Name:         p.Name(),
		DosageForm:   p.DosageForm(),
		PackSize:     p.PackSize(),
		HSNCode:      p.HSNCode(),
		GSTRateBps:   p.GSTRateBps(),
		Manufacturer: p.Manufacturer(),
		IsActive:     p.IsActive(),
		CreatedAt:    p.CreatedAt(),
		UpdatedAt:    p.UpdatedAt(),
	}
}

func batchToDto(b *batch.Batch) BatchDto {
	return BatchDto{
		ID:                         b.ID().String(),
		ProductID:                  b.ProductID().String(),
		TenantID:                   b.TenantID().String(),
		BatchNumber:                b.BatchNumber(),
		ManufactureDate:            b.ManufactureDate(),
		ExpiryDate:                 b.ExpiryDate(),
		ManufacturerName:           b.ManufacturerName(),
		ManufacturingLicenceNumber: b.ManufacturingLicenceNumber(),
		MRPPaise:                   b.MRPPaise(),
		PurchasePricePaise:         b.PurchasePricePaise(),
		QuantityOnHand:             b.QuantityOnHand(),
		Version:                    b.Version(),
		CreatedAt:                  b.CreatedAt(),
		UpdatedAt:                  b.UpdatedAt(),
	}
}

func movementToDto(m *stockmovement.Movement) MovementDto {
	return MovementDto{
		ID:                  m.ID().String(),
		BatchID:             m.BatchID().String(),
		ProductID:           m.ProductID().String(),
		TenantID:            m.TenantID().String(),
		Type:                string(m.Type()),
		Quantity:            m.Quantity(),
		QuantityOnHandAfter: m.QuantityOnHandAfter(),
		Reason:              m.Reason(),
		ActorMembershipID:   m.ActorMembershipID().String(),
		// *string round-trip preserves absent-vs-empty distinction per
		// ADR 0061 amendment 1 (M3) — domain stores *string.
		SourceReference: m.SourceReference(),
		OccurredAt:      m.OccurredAt(),
	}
}
