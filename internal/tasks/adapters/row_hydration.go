package adapters

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/adapters/db"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// snapshotFromColumns builds a workitem.Snapshot from the shared
// column set returned by every WorkItem SELECT. Every row converter
// passes its own pgtype values; this helper centralises the enum
// parsing + UUID/timestamp conversion.
func snapshotFromColumns(
	idCol, tenantIDCol, assignedToCol, assignedByCol, batchIDCol, createdByCol pgtype.UUID,
	dueAtCol, completedAtCol, cancelledAtCol, createdAtCol pgtype.Timestamptz,
	typeCol, priorityCol, stateCol, title, description, cancellationReason, sourceModule string,
	sourceEntityType, sourceEntityID *string,
) (*workitem.WorkItem, error) {
	itype, err := workitem.ParseType(typeCol)
	if err != nil {
		return nil, fmt.Errorf("tasks repo: stored type %q invalid: %w", typeCol, err)
	}
	priority, err := workitem.ParsePriority(priorityCol)
	if err != nil {
		return nil, fmt.Errorf("tasks repo: stored priority %q invalid: %w", priorityCol, err)
	}
	state, err := workitem.ParseState(stateCol)
	if err != nil {
		return nil, fmt.Errorf("tasks repo: stored state %q invalid: %w", stateCol, err)
	}

	src := workitem.Source{
		Module:     sourceModule,
		EntityType: strFromPgOpt(sourceEntityType),
		EntityID:   strFromPgOpt(sourceEntityID),
	}

	snap := workitem.Snapshot{
		ID:                     workitem.ID(pgconv.UUIDFromPg(idCol).String()),
		TenantID:               tenant.ID(pgconv.UUIDFromPg(tenantIDCol).String()),
		Type:                   itype,
		Priority:               priority,
		State:                  state,
		Title:                  title,
		Description:            description,
		AssignedToMembershipID: uuidStringOrEmpty(assignedToCol),
		AssignedByMembershipID: uuidStringOrEmpty(assignedByCol),
		DueAt:                  pgconv.TimeFromPg(dueAtCol),
		CompletedAt:            pgconv.TimeFromPg(completedAtCol),
		CancelledAt:            pgconv.TimeFromPg(cancelledAtCol),
		CancellationReason:     cancellationReason,
		BatchID:                uuidStringOrEmpty(batchIDCol),
		Source:                 src,
		CreatedAt:              pgconv.TimeFromPg(createdAtCol),
		CreatedByMembershipID:  uuidStringOrEmpty(createdByCol),
	}
	return workitem.UnmarshalFromDB(snap), nil
}

func getByIDRowToWorkItem(row db.GetWorkItemByIDRow) (*workitem.WorkItem, error) {
	return snapshotFromColumns(
		row.ID, row.TenantID, row.AssignedToMembershipID, row.AssignedByMembershipID, row.BatchID, row.CreatedByMembershipID,
		row.DueAt, row.CompletedAt, row.CancelledAt, row.CreatedAt,
		row.Type, row.Priority, row.State, row.Title, row.Description, row.CancellationReason, row.SourceModule,
		row.SourceEntityType, row.SourceEntityID,
	)
}

func openBySourceRowToWorkItem(row db.GetOpenWorkItemBySourceRow) (*workitem.WorkItem, error) {
	return snapshotFromColumns(
		row.ID, row.TenantID, row.AssignedToMembershipID, row.AssignedByMembershipID, row.BatchID, row.CreatedByMembershipID,
		row.DueAt, row.CompletedAt, row.CancelledAt, row.CreatedAt,
		row.Type, row.Priority, row.State, row.Title, row.Description, row.CancellationReason, row.SourceModule,
		row.SourceEntityType, row.SourceEntityID,
	)
}

func listRowToWorkItem(row db.ListWorkItemsPageRow) (*workitem.WorkItem, error) {
	return snapshotFromColumns(
		row.ID, row.TenantID, row.AssignedToMembershipID, row.AssignedByMembershipID, row.BatchID, row.CreatedByMembershipID,
		row.DueAt, row.CompletedAt, row.CancelledAt, row.CreatedAt,
		row.Type, row.Priority, row.State, row.Title, row.Description, row.CancellationReason, row.SourceModule,
		row.SourceEntityType, row.SourceEntityID,
	)
}

func overdueCandidateRowToWorkItem(row db.ListOverdueCandidatesRow) (*workitem.WorkItem, error) {
	return snapshotFromColumns(
		row.ID, row.TenantID, row.AssignedToMembershipID, row.AssignedByMembershipID, row.BatchID, row.CreatedByMembershipID,
		row.DueAt, row.CompletedAt, row.CancelledAt, row.CreatedAt,
		row.Type, row.Priority, row.State, row.Title, row.Description, row.CancellationReason, row.SourceModule,
		row.SourceEntityType, row.SourceEntityID,
	)
}

func purgeCandidateRowToWorkItem(row db.ListPurgeCandidatesRow) (*workitem.WorkItem, error) {
	return snapshotFromColumns(
		row.ID, row.TenantID, row.AssignedToMembershipID, row.AssignedByMembershipID, row.BatchID, row.CreatedByMembershipID,
		row.DueAt, row.CompletedAt, row.CancelledAt, row.CreatedAt,
		row.Type, row.Priority, row.State, row.Title, row.Description, row.CancellationReason, row.SourceModule,
		row.SourceEntityType, row.SourceEntityID,
	)
}
