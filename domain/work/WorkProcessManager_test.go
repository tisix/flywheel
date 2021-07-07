package work_test

import (
	"flywheel/account"
	"flywheel/domain"
	"flywheel/domain/flow"
	"flywheel/domain/namespace"
	"flywheel/domain/state"
	"flywheel/domain/work"
	"flywheel/event"
	"flywheel/persistence"
	"flywheel/testinfra"
	"time"

	"github.com/fundwit/go-commons/types"
	"github.com/jinzhu/gorm"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("WorkProcessManager", func() {
	var (
		workProcessManager *work.WorkProcessManager
		workManager        *work.WorkManager
		testDatabase       *testinfra.TestDatabase
		workflowDetail     *domain.WorkflowDetail
		project1           *domain.Project
	)
	BeforeEach(func() {
		testDatabase = testinfra.StartMysqlTestDatabase("flywheel")
		// migration
		Expect(testDatabase.DS.GormDB().AutoMigrate(&domain.Project{}, &domain.ProjectMember{}, &domain.Work{}, &domain.WorkProcessStep{},
			&domain.Workflow{}, &domain.WorkflowState{}, &domain.WorkflowStateTransition{}).Error).To(BeNil())

		persistence.ActiveDataSourceManager = testDatabase.DS
		var err error
		project1, err = namespace.CreateProject(&domain.ProjectCreating{Name: "project 1", Identifier: "GR1"},
			testinfra.BuildSecCtx(100, domain.ProjectRoleManager+"_1", account.SystemAdminPermission.ID))
		Expect(err).To(BeNil())

		workflowManager := flow.NewWorkflowManager(testDatabase.DS)
		workProcessManager = work.NewWorkProcessManager(testDatabase.DS, workflowManager)
		creation := &flow.WorkflowCreation{Name: "test workflow", ProjectID: project1.ID, StateMachine: domain.GenericWorkflowTemplate.StateMachine}
		workflowDetail, err = workflowManager.CreateWorkflow(creation, testinfra.BuildSecCtx(100, domain.ProjectRoleManager+"_"+project1.ID.String()))
		Expect(err).To(BeNil())

		workManager = work.NewWorkManager(testDatabase.DS, workflowManager)
		event.EventPersistCreateFunc = func(record *event.EventRecord, db *gorm.DB) error {
			return nil
		}
	})
	AfterEach(func() {
		testinfra.StopMysqlTestDatabase(testDatabase)
	})

	Describe("QueryProcessSteps", func() {
		It("should be able to catch db errors", func() {
			secCtx := testinfra.BuildSecCtx(1, domain.ProjectRoleManager+"_"+project1.ID.String())
			work, err := workManager.CreateWork(
				&domain.WorkCreation{Name: "test work1", ProjectID: project1.ID, InitialStateName: domain.StatePending.Name}, secCtx)
			Expect(err).To(BeZero())

			testDatabase.DS.GormDB().DropTable(&domain.WorkProcessStep{})
			results, err := workProcessManager.QueryProcessSteps(&domain.WorkProcessStepQuery{WorkID: work.ID}, secCtx)
			Expect(results).To(BeNil())
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(Equal("Error 1146: Table '" + testDatabase.TestDatabaseName + ".work_process_steps' doesn't exist"))

			testDatabase.DS.GormDB().DropTable(&domain.Work{})
			results, err = workProcessManager.QueryProcessSteps(&domain.WorkProcessStepQuery{WorkID: work.ID}, secCtx)
			Expect(results).To(BeNil())
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(Equal("Error 1146: Table '" + testDatabase.TestDatabaseName + ".works' doesn't exist"))
		})

		It("should return empty when work is not found", func() {
			work, err := workProcessManager.QueryProcessSteps(
				&domain.WorkProcessStepQuery{WorkID: 1}, testinfra.BuildSecCtx(100, domain.ProjectRoleManager+"_1"))
			Expect(err).To(BeNil())
			Expect(len(*work)).To(Equal(0))
		})

		It("should return empty when access without permissions", func() {
			detail, err := workManager.CreateWork(
				&domain.WorkCreation{Name: "test work1", ProjectID: project1.ID, InitialStateName: domain.StatePending.Name},
				testinfra.BuildSecCtx(1, domain.ProjectRoleManager+"_"+project1.ID.String()))
			Expect(err).To(BeZero())

			work, err := workProcessManager.QueryProcessSteps(
				&domain.WorkProcessStepQuery{WorkID: detail.ID}, testinfra.BuildSecCtx(100, domain.ProjectRoleManager+"_2"))
			Expect(err).To(BeNil())
			Expect(len(*work)).To(Equal(0))
		})

		It("should return correct result", func() {
			secCtx := testinfra.BuildSecCtx(1, domain.ProjectRoleManager+"_"+project1.ID.String())
			// will create init process step
			work1, err := workManager.CreateWork(&domain.WorkCreation{Name: "test work1", ProjectID: project1.ID, InitialStateName: domain.StatePending.Name}, secCtx)
			Expect(err).To(BeZero())

			// do transition
			workFlowManager := flow.NewWorkflowManager(testDatabase.DS)
			workProcessManager := work.NewWorkProcessManager(testDatabase.DS, workFlowManager)
			err = workProcessManager.CreateWorkStateTransition(
				&domain.WorkProcessStepCreation{FlowID: workflowDetail.ID, WorkID: work1.ID, FromState: work1.StateName, ToState: domain.StateDoing.Name}, secCtx)
			Expect(err).To(BeNil())

			// add a record should not be query out
			now := types.CurrentTimestamp()
			Expect(testDatabase.DS.GormDB().Create(&domain.WorkProcessStep{WorkID: 3, FlowID: 2,
				StateName: "DOING", StateCategory: state.InProcess, BeginTime: now, EndTime: now}).Error).To(BeNil())

			results, err := workProcessManager.QueryProcessSteps(&domain.WorkProcessStepQuery{WorkID: work1.ID}, secCtx)
			Expect(err).To(BeNil())
			Expect(len(*results)).To(Equal(2))
			step1 := (*results)[0]
			Expect(step1.WorkID).To(Equal(work1.ID))
			Expect(step1.FlowID).To(Equal(work1.FlowID))
			Expect(step1.StateName).To(Equal(domain.StatePending.Name))
			Expect(step1.StateCategory).To(Equal(domain.StatePending.Category))
			Expect(step1.BeginTime.Time().Round(time.Microsecond)).To(Equal(work1.CreateTime.Time().Round(time.Microsecond)))
			Expect(step1.EndTime.Time().Unix()-step1.BeginTime.Time().Unix() >= 0).To(BeTrue())
			Expect(step1.NextStateName).To(Equal("DOING"))
			Expect(step1.NextStateCategory).To(Equal(state.InProcess))
			Expect(step1.CreatorID).To(Equal(secCtx.Identity.ID))
			Expect(step1.CreatorName).To(Equal(secCtx.Identity.Nickname))

			step2 := (*results)[1]
			Expect(step2.WorkID).To(Equal(work1.ID))
			Expect(step2.FlowID).To(Equal(work1.FlowID))
			Expect(step2.StateName).To(Equal(domain.StateDoing.Name))
			Expect(step2.StateCategory).To(Equal(domain.StateDoing.Category))
			Expect(step2.BeginTime).To(Equal(step1.EndTime))
			Expect(step2.EndTime).To(Equal(types.Timestamp{}))
			Expect(step2.NextStateName).To(BeZero())
			Expect(step2.NextStateCategory).To(BeZero())
			Expect(step2.CreatorID).To(Equal(secCtx.Identity.ID))
			Expect(step2.CreatorName).To(Equal(secCtx.Identity.Nickname))

			err = workProcessManager.CreateWorkStateTransition(
				&domain.WorkProcessStepCreation{FlowID: workflowDetail.ID, WorkID: work1.ID, FromState: domain.StateDoing.Name, ToState: domain.StateDone.Name}, secCtx)
			Expect(err).To(BeNil())
			results, err = workProcessManager.QueryProcessSteps(&domain.WorkProcessStepQuery{WorkID: work1.ID}, secCtx)
			Expect(err).To(BeNil())
			Expect(len(*results)).To(Equal(3))

			step2Finished := (*results)[1]
			Expect(step2Finished.WorkID).To(Equal(work1.ID))
			Expect(step2Finished.FlowID).To(Equal(work1.FlowID))
			Expect(step2Finished.StateName).To(Equal(domain.StateDoing.Name))
			Expect(step2Finished.StateCategory).To(Equal(domain.StateDoing.Category))
			Expect(step2Finished.BeginTime).To(Equal(step1.EndTime))
			Expect(step2Finished.EndTime.Time().Unix()-step2Finished.BeginTime.Time().Unix() >= 0).To(BeTrue())
			Expect(step2Finished.NextStateName).To(Equal(domain.StateDone.Name))
			Expect(step2Finished.NextStateCategory).To(Equal(domain.StateDone.Category))
			Expect(step2Finished.CreatorID).To(Equal(secCtx.Identity.ID))
			Expect(step2Finished.CreatorName).To(Equal(secCtx.Identity.Nickname))

			step3 := (*results)[2]
			Expect(step3.WorkID).To(Equal(work1.ID))
			Expect(step3.FlowID).To(Equal(work1.FlowID))
			Expect(step3.StateName).To(Equal(domain.StateDone.Name))
			Expect(step3.StateCategory).To(Equal(domain.StateDone.Category))
			Expect(step3.BeginTime).To(Equal(step2Finished.EndTime))
			Expect(step3.EndTime).To(BeZero())
			Expect(step3.NextStateName).To(BeZero())
			Expect(step3.NextStateCategory).To(BeZero())
			Expect(step3.CreatorID).To(Equal(secCtx.Identity.ID))
			Expect(step3.CreatorName).To(Equal(secCtx.Identity.Nickname))
		})
	})
})
