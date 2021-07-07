package flow

import (
	"errors"
	"flywheel/bizerror"
	"flywheel/domain"
	"flywheel/domain/state"
	"flywheel/idgen"
	"flywheel/persistence"
	"flywheel/session"
	"strconv"
	"time"

	"github.com/fundwit/go-commons/types"
	"github.com/jinzhu/gorm"
	"github.com/sony/sonyflake"
)

type WorkflowManagerTraits interface {
	QueryWorkflows(query *domain.WorkflowQuery, sec *session.Context) (*[]domain.Workflow, error)
	CreateWorkflow(c *WorkflowCreation, sec *session.Context) (*domain.WorkflowDetail, error)
	DetailWorkflow(ID types.ID, sec *session.Context) (*domain.WorkflowDetail, error)
	UpdateWorkflowBase(ID types.ID, c *WorkflowBaseUpdation, sec *session.Context) (*domain.Workflow, error)
	DeleteWorkflow(ID types.ID, sec *session.Context) error

	// updateStateMachine
	CreateWorkflowStateTransitions(id types.ID, transitions []state.Transition, sec *session.Context) error
	DeleteWorkflowStateTransitions(id types.ID, transitions []state.Transition, sec *session.Context) error

	CreateState(workflowID types.ID, creating *StateCreating, sec *session.Context) error
	UpdateWorkflowState(id types.ID, updating WorkflowStateUpdating, sec *session.Context) error
	UpdateStateRangeOrders(workflowID types.ID, wantedOrders *[]StateOrderRangeUpdating, sec *session.Context) error
}

type WorkflowManager struct {
	dataSource *persistence.DataSourceManager
	idWorker   *sonyflake.Sonyflake
}

type WorkflowCreation struct {
	Name       string   `json:"name"       binding:"required"`
	ProjectID  types.ID `json:"projectId"    binding:"required"`
	ThemeColor string   `json:"themeColor" binding:"required"`
	ThemeIcon  string   `json:"themeIcon"  binding:"required"`

	StateMachine state.StateMachine `json:"stateMachine" binding:"dive"`
}

type WorkflowBaseUpdation struct {
	Name       string `json:"name"     binding:"required"`
	ThemeColor string `json:"themeColor" binding:"required"`
	ThemeIcon  string `json:"themeIcon"  binding:"required"`
}

type WorkflowStateUpdating struct {
	OriginName string `json:"originName"  binding:"required"`

	Name  string `json:"name"        binding:"required"`
	Order int    `json:"order"`
}

type StateOrderRangeUpdating struct {
	State    string `json:"state" validate:"required"`
	NewOlder int    `json:"newOrder"`
	OldOlder int    `json:"oldOrder"`
}
type StateCreating struct {
	Name        string             `json:"name"         binding:"required"`
	Category    state.Category     `json:"category"     binding:"required"`
	Order       int                `json:"order"        binding:"required"`
	Transitions []state.Transition `json:"transitions"  binding:"dive"`
}

func NewWorkflowManager(ds *persistence.DataSourceManager) *WorkflowManager {
	return &WorkflowManager{
		dataSource: ds,
		idWorker:   sonyflake.NewSonyflake(sonyflake.Settings{}),
	}
}

func (m *WorkflowManager) CreateWorkflow(c *WorkflowCreation, sec *session.Context) (*domain.WorkflowDetail, error) {
	if !sec.HasRoleSuffix("_" + c.ProjectID.String()) {
		return nil, bizerror.ErrForbidden
	}

	workflow := &domain.WorkflowDetail{
		Workflow: domain.Workflow{
			ID:         idgen.NextID(m.idWorker),
			Name:       c.Name,
			ProjectID:  c.ProjectID,
			ThemeColor: c.ThemeColor,
			ThemeIcon:  c.ThemeIcon,
			CreateTime: time.Now().Round(time.Millisecond),
		},
		StateMachine: c.StateMachine,
	}

	stateNum := len(workflow.StateMachine.States)
	for idx := 0; idx < stateNum; idx++ {
		workflow.StateMachine.States[idx].Order = 10000 + idx + 1
	}

	db := m.dataSource.GormDB()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(workflow.Workflow).Error; err != nil {
			return err
		}
		for _, s := range workflow.StateMachine.States {
			stateEntity := &domain.WorkflowState{
				WorkflowID: workflow.ID, Order: s.Order, Name: s.Name, Category: s.Category, CreateTime: workflow.CreateTime,
			}
			if err := tx.Create(stateEntity).Error; err != nil {
				return err
			}
		}
		for _, t := range workflow.StateMachine.Transitions {
			transition := &domain.WorkflowStateTransition{
				WorkflowID: workflow.ID, Name: t.Name, FromState: t.From, ToState: t.To, CreateTime: workflow.CreateTime,
			}
			if err := tx.Create(transition).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return workflow, nil
}

func (m *WorkflowManager) DetailWorkflow(id types.ID, sec *session.Context) (*domain.WorkflowDetail, error) {
	workflowDetail := domain.WorkflowDetail{}
	db := m.dataSource.GormDB()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(&domain.Workflow{ID: id}).First(&(workflowDetail.Workflow)).Error; err != nil {
			return err
		}
		if !sec.HasRoleSuffix("_" + workflowDetail.ProjectID.String()) {
			return bizerror.ErrForbidden
		}

		var stateRecords []domain.WorkflowState
		if err := tx.Where(domain.WorkflowState{WorkflowID: workflowDetail.ID}).Order("`order` ASC").Find(&stateRecords).Error; err != nil {
			return err
		}
		var transitionRecords []domain.WorkflowStateTransition
		if err := tx.Where(domain.WorkflowStateTransition{WorkflowID: workflowDetail.ID}).Find(&transitionRecords).Error; err != nil {
			return err
		}
		stateMachine := state.StateMachine{}
		for _, record := range stateRecords {
			stateMachine.States = append(stateMachine.States, state.State{Name: record.Name, Category: record.Category, Order: record.Order})
		}
		for _, record := range transitionRecords {
			from, fromStateFound := stateMachine.FindState(record.FromState)
			to, toStateFound := stateMachine.FindState(record.ToState)
			if !fromStateFound || !toStateFound {
				return bizerror.ErrStateInvalid
			}
			stateMachine.Transitions = append(stateMachine.Transitions, state.Transition{Name: record.Name, From: from.Name, To: to.Name})
		}

		workflowDetail.StateMachine = stateMachine
		return nil
	})

	if err != nil {
		return nil, err
	}

	return &workflowDetail, nil
}

func (m *WorkflowManager) QueryWorkflows(query *domain.WorkflowQuery, sec *session.Context) (*[]domain.Workflow, error) {
	var workflows []domain.Workflow
	db := m.dataSource.GormDB()

	q := db.Where(domain.Workflow{ProjectID: query.ProjectID})
	if query.Name != "" {
		q = q.Where("name like ?", "%"+query.Name+"%")
	}
	visibleProjects := sec.VisibleProjects()
	if len(visibleProjects) == 0 {
		return &[]domain.Workflow{}, nil
	}
	q = q.Where("project_id in (?)", visibleProjects)
	if err := q.Find(&workflows).Error; err != nil {
		return nil, err
	}

	return &workflows, nil
}

func (m *WorkflowManager) UpdateWorkflowBase(id types.ID, c *WorkflowBaseUpdation, sec *session.Context) (*domain.Workflow, error) {
	wf := domain.Workflow{}
	db := m.dataSource.GormDB()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(&domain.Workflow{ID: id}).First(&wf).Error; err != nil {
			return err
		}
		if !sec.HasRoleSuffix(domain.ProjectRoleManager + "_" + wf.ProjectID.String()) {
			return bizerror.ErrForbidden
		}
		if err := tx.Model(&domain.Workflow{}).Where(&domain.Workflow{ID: id}).
			Update(&domain.Workflow{Name: c.Name, ThemeIcon: c.ThemeIcon, ThemeColor: c.ThemeColor}).Error; err != nil {
			return err
		}
		// query again
		if err := tx.Where(&domain.Workflow{ID: id}).First(&wf).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &wf, nil
}

func (m *WorkflowManager) DeleteWorkflow(id types.ID, sec *session.Context) error {
	wf := domain.Workflow{}
	db := m.dataSource.GormDB()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(&domain.Workflow{ID: id}).First(&wf).Error; err != nil {
			return err
		}
		if !sec.HasRoleSuffix(domain.ProjectRoleManager + "_" + wf.ProjectID.String()) {
			return bizerror.ErrForbidden
		}

		if err := isWorkflowReferenced(tx, wf.ID); err != nil {
			return err
		}

		if err := tx.Model(&domain.Workflow{}).Delete(&domain.Workflow{ID: id}).Error; err != nil {
			return err
		}
		if err := tx.Model(&domain.WorkflowState{}).Where("workflow_id = ?", wf.ID).
			Delete(&domain.WorkflowState{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&domain.WorkflowStateTransition{}).Where("workflow_id = ?", wf.ID).
			Delete(&domain.WorkflowStateTransition{}).Error; err != nil {
			return err
		}
		return nil
	})
	return err
}

func (m *WorkflowManager) CreateWorkflowStateTransitions(id types.ID, transitions []state.Transition, sec *session.Context) error {
	workflow := domain.Workflow{}
	db := m.dataSource.GormDB()
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(&domain.Workflow{ID: id}).First(&workflow).Error; err != nil {
			return err
		}
		if !sec.HasRoleSuffix(domain.ProjectRoleManager + "_" + workflow.ProjectID.String()) {
			return bizerror.ErrForbidden
		}

		var states []domain.WorkflowState
		if err := tx.Where(domain.WorkflowState{WorkflowID: workflow.ID}).Find(&states).Error; err != nil {
			return err
		}
		stateIndex := map[string]domain.WorkflowState{}
		for _, t := range states {
			stateIndex[t.Name] = t
		}

		for _, t := range transitions {
			if _, found := stateIndex[t.From]; !found {
				return bizerror.ErrUnknownState
			}
			if _, found := stateIndex[t.To]; !found {
				return bizerror.ErrUnknownState
			}
			transition := &domain.WorkflowStateTransition{
				WorkflowID: workflow.ID, Name: t.Name, FromState: t.From, ToState: t.To, CreateTime: time.Now(),
			}
			if err := tx.Save(transition).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *WorkflowManager) DeleteWorkflowStateTransitions(id types.ID, transitions []state.Transition, sec *session.Context) error {
	wf := domain.Workflow{}
	db := m.dataSource.GormDB()
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(&domain.Workflow{ID: id}).First(&wf).Error; err != nil {
			return err
		}
		if !sec.HasRoleSuffix(domain.ProjectRoleManager + "_" + wf.ProjectID.String()) {
			return bizerror.ErrForbidden
		}

		for _, t := range transitions {
			q := tx.Model(&domain.WorkflowStateTransition{}).
				Where("workflow_id = ?", wf.ID).
				Where("from_state LIKE ?", t.From).
				Where("to_state LIKE ?", t.To)
			if err := q.Delete(&domain.WorkflowStateTransition{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *WorkflowManager) UpdateWorkflowState(id types.ID, updating WorkflowStateUpdating, sec *session.Context) error {
	workflow := domain.Workflow{}
	db := m.dataSource.GormDB()
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(&domain.Workflow{ID: id}).First(&workflow).Error; err != nil {
			return err
		}
		if !sec.HasRoleSuffix(domain.ProjectRoleManager + "_" + workflow.ProjectID.String()) {
			return bizerror.ErrForbidden
		}

		// origin state must exist
		var originState domain.WorkflowState
		if err := tx.Where(domain.WorkflowState{WorkflowID: workflow.ID, Name: updating.OriginName}).First(&originState).Error; err != nil {
			return err
		}

		if updating.OriginName != updating.Name {
			// new state name must not exist
			var existState []domain.WorkflowState
			if err := tx.Where(domain.WorkflowState{WorkflowID: workflow.ID, Name: updating.Name}).First(&existState).Error; err != nil {
				return err
			}
			if len(existState) > 0 {
				return bizerror.ErrStateExisted
			}
		}

		// delete origin state
		if err := tx.Model(originState).Delete(originState).Error; err != nil {
			return err
		}
		// insert new state
		stateEntity := &domain.WorkflowState{
			WorkflowID: workflow.ID, Order: updating.Order, Name: updating.Name, Category: originState.Category, CreateTime: workflow.CreateTime,
		}
		if err := tx.Create(stateEntity).Error; err != nil {
			return err
		}

		// update referrers
		if originState.Name != updating.Name {
			// workflow_state_transitions:    workflow_id, from_state, to_state
			if err := tx.Model(&domain.WorkflowStateTransition{}).
				Where("workflow_id = ?", originState.WorkflowID).
				Where("from_state LIKE ?", originState.Name).
				Update(domain.WorkflowStateTransition{FromState: updating.Name}).Error; err != nil {
				return err
			}
			if err := tx.Model(&domain.WorkflowStateTransition{}).
				Where("workflow_id = ?", originState.WorkflowID).
				Where("to_state LIKE ?", originState.Name).
				Update(domain.WorkflowStateTransition{ToState: updating.Name}).Error; err != nil {
				return err
			}
		}
		if originState.Name != updating.Name {
			// work:  flow_id, state_name  state_category
			if err := tx.Model(&domain.Work{}).
				Where("flow_id = ?", originState.WorkflowID).
				Where("state_name LIKE ?", originState.Name).
				Update(domain.Work{StateName: updating.Name, StateCategory: originState.Category}).Error; err != nil {
				return err
			}

			// work_process_steps: flow_id, state_name, state_category, next_state_name, next_state_category
			if err := tx.Model(&domain.WorkProcessStep{}).
				Where("flow_id = ?", originState.WorkflowID).
				Where("state_name LIKE ?", originState.Name).
				Update(domain.WorkProcessStep{StateName: updating.Name, StateCategory: originState.Category}).Error; err != nil {
				return err
			}

			if err := tx.Model(&domain.WorkProcessStep{}).
				Where("flow_id = ?", originState.WorkflowID).
				Where("next_state_name LIKE ?", originState.Name).
				Update(domain.WorkProcessStep{NextStateName: updating.Name, NextStateCategory: originState.Category}).Error; err != nil {
				return err
			}
		}

		return nil
	})
}

func (m *WorkflowManager) CreateState(workflowID types.ID, creating *StateCreating, sec *session.Context) error {
	now := time.Now()
	return m.dataSource.GormDB().Transaction(func(tx *gorm.DB) error {
		if err := m.checkPerms(workflowID, sec); err != nil {
			return err
		}

		stateEntity := &domain.WorkflowState{
			WorkflowID: workflowID, Order: creating.Order, Name: creating.Name, Category: creating.Category, CreateTime: now,
		}
		if err := tx.Create(stateEntity).Error; err != nil {
			return err
		}

		var stateRecords []domain.WorkflowState
		if err := tx.Where(domain.WorkflowState{WorkflowID: workflowID}).Order("`order` ASC").Find(&stateRecords).Error; err != nil {
			return err
		}
		stateMap := map[string]string{}
		for _, stateRecord := range stateRecords {
			stateMap[stateRecord.Name] = stateRecord.Name
		}

		for _, t := range creating.Transitions {
			if stateMap[t.From] == "" || stateMap[t.To] == "" {
				return bizerror.ErrUnknownState
			}

			transition := &domain.WorkflowStateTransition{
				WorkflowID: workflowID, Name: t.Name, FromState: t.From, ToState: t.To, CreateTime: now,
			}
			if err := tx.Create(transition).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *WorkflowManager) UpdateStateRangeOrders(workflowID types.ID, wantedOrders *[]StateOrderRangeUpdating, sec *session.Context) error {
	if wantedOrders == nil || len(*wantedOrders) == 0 {
		return nil
	}

	return m.dataSource.GormDB().Transaction(func(tx *gorm.DB) error {
		if err := m.checkPerms(workflowID, sec); err != nil {
			return err
		}

		for _, orderUpdating := range *wantedOrders {
			db := tx.Model(&domain.WorkflowState{}).
				Where(&domain.WorkflowState{WorkflowID: workflowID, Name: orderUpdating.State}).
				Update("order", orderUpdating.NewOlder)
			if err := db.Error; err != nil {
				return err
			}
			if db.RowsAffected != 1 {
				return errors.New("expected affected row is 1, but actual is " + strconv.FormatInt(db.RowsAffected, 10))
			}
		}
		return nil
	})
}

func (m *WorkflowManager) checkPerms(id types.ID, sec *session.Context) error {
	var workflow domain.Workflow
	if err := m.dataSource.GormDB().Where(&domain.Workflow{ID: id}).First(&workflow).Error; err != nil {
		return err
	}
	if sec == nil || !sec.HasRoleSuffix("_"+workflow.ProjectID.String()) {
		return bizerror.ErrForbidden
	}
	return nil
}

func isWorkflowReferenced(db *gorm.DB, workflowID types.ID) error {
	var work domain.Work
	err := db.Model(&domain.Work{}).Where(&domain.Work{FlowID: workflowID}).First(&work).Error
	if err == nil {
		return bizerror.ErrWorkflowIsReferenced
	}
	if err != gorm.ErrRecordNotFound {
		return err
	}

	var workProcessStep domain.WorkProcessStep
	err = db.Model(&domain.WorkProcessStep{}).Where(&domain.WorkProcessStep{FlowID: workflowID}).First(&workProcessStep).Error
	if err == nil {
		return bizerror.ErrWorkflowIsReferenced
	}
	if err != gorm.ErrRecordNotFound {
		return err
	}
	return nil
}
