package ports

import (
	"encoding/json"
	"errors"
	"github.com/leadkart/leadkart-go/internal/common/lifecycle"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
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
	mux.Handle("POST /api/v1/inventory/products/{productId}/deactivate",
		catalogManage(handleChangeProductStatus(log, a, false)))
	mux.Handle("POST /api/v1/inventory/products/{productId}/reactivate",
		catalogManage(handleChangeProductStatus(log, a, true)))
	mux.Handle("GET /api/v1/inventory/product-categories",
		catalogRead(handleListProductCategories(log, a)))

	// ----- Batches ------------------------------------------------------------
	mux.Handle("POST /api/v1/inventory/products/{productId}/batches",
		stockManage(handleAddBatch(log, a)))
	mux.Handle("GET /api/v1/inventory/products/{productId}/batches",
		stockRead(handleListBatchesForProduct(log, a)))
	mux.Handle("GET /api/v1/inventory/products/{productId}/fefo-batches",
		stockRead(handleFefoBatches(log, a)))
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
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		var req CreateProductRequest
		if !decodeStrict(w, r, &req) {
			return
		}
		if errs := productFieldErrors(req.ProductCategory, req.ReorderLevel, req.ExpiryAlertThresholdDays); errs != nil {
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, errs)
			return
		}
		out, err := a.Commands.CreateProduct.Handle(r.Context(), command.CreateProductCommand{
			TenantID:                 tenant.ID(effTenant),
			ActorMembershipID:        membership.ID(c.MembershipID),
			SKU:                      req.SKU,
			Name:                     req.Name,
			DosageForm:               req.DosageForm,
			PackSize:                 req.PackSize,
			HSNCode:                  req.HSNCode,
			GSTRateID:                strings.TrimSpace(req.GSTRateID),
			Manufacturer:             req.Manufacturer,
			DefaultSaleRatePaise:     derefOr(req.DefaultSaleRatePaise, 0),
			GenericName:              derefOr(req.GenericName, ""),
			Composition:              derefOr(req.Composition, ""),
			StorageCondition:         derefOr(req.StorageCondition, ""),
			DrugSchedule:             strings.TrimSpace(derefOr(req.DrugSchedule, "")),
			DrugScheduleGiven:        req.DrugSchedule != nil,
			ProductCategory:          derefOr(req.ProductCategory, ""),
			ReorderLevel:             derefOr(req.ReorderLevel, 0),
			ExpiryAlertThresholdDays: req.ExpiryAlertThresholdDays,
		})
		var refusal *command.FieldRefusal
		switch {
		case errors.As(err, &refusal):
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, refusal.Fields)
			return
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
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		_, ok = authn.ClaimsFromContext(r.Context())
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
		status, ok := lifecycle.ParseStatusFilter(q.Get("status"))
		if !ok {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidStatusFilter, "status must be one of active, inactive, all")
			return
		}
		// A filter is text sent to PostgreSQL, which cannot take U+0000 (%00).
		for _, name := range []string{"dosage_form", "manufacturer", "search"} {
			if strings.IndexByte(q.Get(name), 0) >= 0 {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidQuery, name+" must not contain the NUL character")
				return
			}
		}
		filter := query.ProductListFilter{
			Status:       status,
			DosageForm:   strings.TrimSpace(q.Get("dosage_form")),
			Manufacturer: strings.TrimSpace(q.Get("manufacturer")),
			Search:       strings.TrimSpace(q.Get("search")),
		}
		if errs := productStockFilters(q, &filter); errs != nil {
			writeFieldErrors(w, http.StatusBadRequest, ErrCodeInvalidQuery, errs)
			return
		}
		page, err := a.Queries.ListProductsPage.Handle(r.Context(), query.ListProductsPageQuery{
			TenantID: tenant.ID(effTenant),
			Cursor:   cursor,
			PageSize: pageSize,
			Filter:   filter,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list products failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		items := make([]ProductDto, 0, len(page.Items))
		for _, v := range page.Items {
			items = append(items, productViewToDto(v))
		}
		out := ListProductsResponse{Items: items, HasMore: page.HasMore, NextCursor: page.NextCursor}
		// The status chips' counts ride the FIRST page only (no cursor).
		if cursor.ID == "" && cursor.SortValue.IsZero() {
			counts, cerr := a.Queries.CountProductsByStatus.Handle(r.Context(), query.CountProductsByStatusQuery{TenantID: tenant.ID(effTenant)})
			if cerr != nil {
				log.ErrorContext(r.Context(), "count products by status failed", "err", cerr)
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
				return
			}
			out.StatusCounts = &counts
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handleGetProduct(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		_, ok = authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		id, ok := parsePathUUID(w, r, "productId")
		if !ok {
			return
		}
		v, err := a.Queries.GetProduct.Handle(r.Context(), query.GetProductQuery{
			TenantID:  tenant.ID(effTenant),
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
		writeJSON(w, http.StatusOK, productViewToDto(v))
	})
}

func handleUpdateProduct(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
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
		if !decodeStrict(w, r, &req) {
			return
		}
		if errs := productFieldErrors(req.ProductCategory, req.ReorderLevel, req.ExpiryAlertThresholdDays); errs != nil {
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, errs)
			return
		}
		err := a.Commands.UpdateProduct.Handle(r.Context(), command.UpdateProductCommand{
			TenantID:                 tenant.ID(effTenant),
			ProductID:                product.ID(id.String()),
			ActorMembershipID:        membership.ID(c.MembershipID),
			Name:                     req.Name,
			GSTRateID:                trimmed(req.GSTRateID),
			Manufacturer:             req.Manufacturer,
			DefaultSaleRatePaise:     req.DefaultSaleRatePaise,
			GenericName:              req.GenericName,
			Composition:              req.Composition,
			StorageCondition:         req.StorageCondition,
			DrugSchedule:             trimmed(req.DrugSchedule),
			ReorderLevel:             req.ReorderLevel,
			ExpiryAlertThresholdDays: req.ExpiryAlertThresholdDays,
			ProductCategory:          req.ProductCategory,
		})
		var refusal *command.FieldRefusal
		switch {
		case errors.As(err, &refusal):
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, refusal.Fields)
			return
		case errors.Is(err, product.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "")
			return
		case errors.Is(err, product.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "update product failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// handleChangeProductStatus serves POST .../deactivate (reactivate=false) and .../reactivate
// (reactivate=true). A product is never deleted (ADR 0073): both answer 200 with the updated
// product, and repeating either is a no-op that answers the same.
func handleChangeProductStatus(log *slog.Logger, a app.Application, reactivate bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		id, ok := parsePathUUID(w, r, "productId")
		if !ok {
			return
		}
		cmd := command.ChangeProductStatusCommand{
			TenantID:          tenant.ID(effTenant),
			ProductID:         product.ID(id.String()),
			ActorMembershipID: membership.ID(c.MembershipID),
		}
		var err error
		if reactivate {
			err = a.Commands.ReactivateProduct.Handle(r.Context(), cmd)
		} else {
			err = a.Commands.DeactivateProduct.Handle(r.Context(), cmd)
		}
		switch {
		case errors.Is(err, product.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "")
			return
		case errors.Is(err, product.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "change product status failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		v, err := a.Queries.GetProduct.Handle(r.Context(), query.GetProductQuery{TenantID: cmd.TenantID, ProductID: cmd.ProductID})
		if err != nil {
			log.ErrorContext(r.Context(), "read product after status change failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, productViewToDto(v))
	})
}

// handleListProductCategories serves the product form's category list: every
// category of the shared reference table with its default GST rate.
func handleListProductCategories(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authn.EffectiveTenantID(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		views, err := a.Queries.ListProductCategories.Handle(r.Context(), query.ListProductCategoriesQuery{})
		if err != nil {
			log.ErrorContext(r.Context(), "list product categories failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		items := make([]ProductCategoryDto, 0, len(views))
		for _, v := range views {
			items = append(items, ProductCategoryDto{Category: v.Category})
		}
		writeJSON(w, http.StatusOK, ListProductCategoriesResponse{Items: items})
	})
}

func handleAddBatch(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
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
		if !decodeStrict(w, r, &req) {
			return
		}
		if req.InitialQuantity != nil && *req.InitialQuantity < 0 {
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed,
				map[string][]string{"initial_quantity": {ruleTooSmall}})
			return
		}
		expiryDate, err := expiry.ParsePrintedMonth(req.ExpiryMonth)
		if err != nil {
			code := ruleInvalidFormat
			if strings.TrimSpace(req.ExpiryMonth) == "" {
				code = ruleRequired
			}
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed,
				map[string][]string{"expiry_month": {code}})
			return
		}
		out, err := a.Commands.AddBatch.Handle(r.Context(), command.AddBatchCommand{
			TenantID:                   tenant.ID(effTenant),
			ProductID:                  product.ID(productID.String()),
			ActorMembershipID:          membership.ID(c.MembershipID),
			BatchNumber:                req.BatchNumber,
			ManufactureDate:            req.ManufactureDate,
			ExpiryDate:                 expiryDate,
			ManufacturerName:           req.ManufacturerName,
			ManufacturingLicenceNumber: req.ManufacturingLicenceNumber,
			MRPPaise:                   req.MRPPaise,
			PurchasePricePaise:         req.PurchasePricePaise,
			InitialQuantity:            derefOr(req.InitialQuantity, 0),
		})
		switch {
		case errors.Is(err, product.ErrInactive):
			writeError(w, http.StatusConflict, ErrCodeProductInactive, "the product is inactive; reactivate it before adding a batch")
			return
		case errors.Is(err, product.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "product not found")
			return
		case errors.Is(err, batch.ErrBatchNumberTaken):
			writeError(w, http.StatusConflict, ErrCodeAlreadyExists, "batch_number already exists for this product")
			return
		case errors.Is(err, batch.ErrExpired):
			// Only reachable with initial_quantity: stock cannot be received
			// into a batch that is already expired. Nothing was written.
			writeError(w, http.StatusConflict, ErrCodeBatchExpired,
				"stock cannot be received into an expired batch; add the batch without initial_quantity")
			return
		case errors.Is(err, batch.ErrManufactureDateInFuture):
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed,
				map[string][]string{"manufacture_date": {ruleInFuture}})
			return
		case errors.Is(err, batch.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "add batch failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, AddBatchResponse{BatchID: out.BatchID.String(), Warnings: nonNil(out.Warnings)})
	})
}

func handleListBatchesForProduct(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		_, ok = authn.ClaimsFromContext(r.Context())
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
			TenantID:       tenant.ID(effTenant),
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
		for _, v := range page.Items {
			items = append(items, batchViewToDto(v))
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
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		_, ok = authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		id, ok := parsePathUUID(w, r, "batchId")
		if !ok {
			return
		}
		v, err := a.Queries.GetBatch.Handle(r.Context(), query.GetBatchQuery{
			TenantID: tenant.ID(effTenant),
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
		writeJSON(w, http.StatusOK, batchViewToDto(v))
	})
}

func handleLogStockMovement(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
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
		if !decodeStrict(w, r, &req) {
			return
		}
		// Reservation and release are the order saga's bookkeeping, applied by
		// the order-event handlers; a person must not forge or drop an
		// order's hold here. Only the three manual types are public.
		if !isPublicMovementType(batch.MovementType(req.Type)) {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidMovementType,
				"type must be one of inbound, outbound, adjustment, quarantine, release_quarantine, write_off, return_to_maker")
			return
		}
		out, err := a.Commands.LogStockMovement.Handle(r.Context(), command.LogStockMovementCommand{
			TenantID:          tenant.ID(effTenant),
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
		case errors.Is(err, batch.ErrInsufficientStock), errors.Is(err, batch.ErrInsufficientFree):
			writeError(w, http.StatusConflict, ErrCodeInsufficientStock, err.Error())
			return
		case errors.Is(err, batch.ErrInsufficientQuarantined):
			writeError(w, http.StatusConflict, ErrCodeInsufficientQuarantined, err.Error())
			return
		case errors.Is(err, batch.ErrExpired):
			writeError(w, http.StatusConflict, ErrCodeBatchExpired, err.Error())
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
			MovementID:               out.MovementID.String(),
			QuantityOnHandAfter:      out.QuantityOnHandAfter,
			QuantityQuarantinedAfter: out.QuantityQuarantinedAfter,
			Warnings:                 nonNil(out.Warnings),
		})
	})
}

func handleListBatchMovements(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		_, ok = authn.ClaimsFromContext(r.Context())
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
			TenantID: tenant.ID(effTenant),
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
		for _, v := range page.Items {
			items = append(items, movementViewToDto(v))
		}
		writeJSON(w, http.StatusOK, ListMovementsResponse{
			Items:      items,
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		})
	})
}

func handleFefoBatches(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		_, ok = authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		productID, ok := parsePathUUID(w, r, "productId")
		if !ok {
			return
		}
		batches, err := a.Queries.FefoBatches.Handle(r.Context(), query.FefoBatchesQuery{
			TenantID:  tenant.ID(effTenant),
			ProductID: product.ID(productID.String()),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "fefo batches failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		items := make([]BatchDto, 0, len(batches))
		for _, b := range batches {
			items = append(items, batchViewToDto(b))
		}
		writeJSON(w, http.StatusOK, FefoBatchesResponse{Items: items})
	})
}

// ----- Helpers --------------------------------------------------------------

// parsePathUUID extracts + validates a UUID path value; writes 400 +
// returns (uuid.Nil, false) on bad input.
func parsePathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := r.PathValue(name)
	canon, ok := authn.CanonicalID(raw)
	if ok {
		if id, err := uuid.Parse(canon); err == nil {
			return id, true
		}
	}
	writeError(w, http.StatusBadRequest, ErrCodeInvalidID,
		name+" path parameter must be a UUID")
	return uuid.Nil, false
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

// trimmed returns a pointer to the trimmed value, or nil when p is nil.
func trimmed(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	return &v
}

// derefOr returns *p, or fallback when the request did not send the field.
func derefOr[T comparable](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
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

// productViewToDto maps the app-layer read View to the wire DTO (1:1).
// Per STRICT CQRS the write aggregate never reaches the port — the
// projection lives in query.projectProduct; this is a trivial copy.
func productViewToDto(v query.ProductView) ProductDto {
	return ProductDto{
		ID:           v.ID,
		TenantID:     v.TenantID,
		SKU:          v.SKU,
		Name:         v.Name,
		DosageForm:   v.DosageForm,
		PackSize:     v.PackSize,
		HSNCode:      v.HSNCode,
		GSTRateBps:   v.GSTRateBps,
		Manufacturer: v.Manufacturer,
		IsActive:     v.IsActive,
		CreatedAt:    v.CreatedAt,
		UpdatedAt:    v.UpdatedAt,

		ProductCategory:          v.ProductCategory,
		ReorderLevel:             v.ReorderLevel,
		ExpiryAlertThresholdDays: v.ExpiryAlertThresholdDays,

		GSTRateID:            v.GSTRateID,
		GSTRateLabel:         v.GSTRateLabel,
		DefaultSaleRatePaise: v.DefaultSaleRatePaise,
		GenericName:          v.GenericName,
		Composition:          v.Composition,
		StorageCondition:     v.StorageCondition,
		DrugSchedule:         v.DrugScheduleClassID,
		DrugScheduleLabel:    v.DrugScheduleLabel,

		StockOnHand:      v.StockOnHand,
		StockReserved:    v.StockReserved,
		StockAvailable:   v.StockAvailable,
		StockUnsellable:  v.StockUnsellable,
		StockQuarantined: v.StockQuarantined,
		EarliestExpiry:   v.EarliestExpiry,
	}
}

// batchViewToDto maps the app-layer read View to the wire DTO (1:1).
func batchViewToDto(v query.BatchView) BatchDto {
	return BatchDto{
		ID:                         v.ID,
		ProductID:                  v.ProductID,
		TenantID:                   v.TenantID,
		BatchNumber:                v.BatchNumber,
		ManufactureDate:            v.ManufactureDate,
		ExpiryMonth:                expiry.PrintedMonth(v.ExpiryDate),
		ExpiryDate:                 v.ExpiryDate,
		ManufacturerName:           v.ManufacturerName,
		ManufacturingLicenceNumber: v.ManufacturingLicenceNumber,
		MRPPaise:                   v.MRPPaise,
		PurchasePricePaise:         v.PurchasePricePaise,
		QuantityOnHand:             v.QuantityOnHand,
		QuantityReserved:           v.QuantityReserved,
		QuantityQuarantined:        v.QuantityQuarantined,
		QuantityAvailable:          v.QuantityAvailable,
		QuantityUnsellable:         v.QuantityUnsellable,
		Sellable:                   v.Sellable,
		LastSellableDay:            v.LastSellableDay.Format(time.DateOnly),
		PastPrintedExpiry:          v.PastPrintedExpiry,
		Version:                    v.Version,
		CreatedAt:                  v.CreatedAt,
		UpdatedAt:                  v.UpdatedAt,
	}
}

// movementViewToDto maps the app-layer read View to the wire DTO (1:1).
func movementViewToDto(v query.MovementView) MovementDto {
	return MovementDto{
		ID:                  v.ID,
		BatchID:             v.BatchID,
		ProductID:           v.ProductID,
		TenantID:            v.TenantID,
		Type:                v.Type,
		Quantity:            v.Quantity,
		QuantityOnHandAfter: v.QuantityOnHandAfter,
		Reason:              v.Reason,
		ActorMembershipID:   v.ActorMembershipID,
		// *string round-trip preserves absent-vs-empty distinction per
		// ADR 0061 amendment 1 (M3) — View stores *string.
		SourceReference: v.SourceReference,
		OccurredAt:      v.OccurredAt,
	}
}

// nonNil returns a non-nil slice so a JSON list is [] and never null.
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// isPublicMovementType reports whether t may be logged through the public
// movement route: the manual types (stock in or out, a count correction, and the
// quarantine trio), never the saga-owned reservation and release.
func isPublicMovementType(t batch.MovementType) bool {
	switch t {
	case batch.MovementInbound, batch.MovementOutbound, batch.MovementAdjustment,
		batch.MovementQuarantine, batch.MovementReleaseQuarantine, batch.MovementWriteOff, batch.MovementReturnToMaker:
		return true
	default:
		return false
	}
}
