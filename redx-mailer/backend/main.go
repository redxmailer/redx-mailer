package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// ==================================================
// ENVIRONMENT VARIABLES
// ==================================================
type Config struct {
	Port                  string
	SpreadsheetID         string
	SupabaseURL           string
	SupabaseServiceKey    string
	GoogleServiceAccount  string
	AllowOrigin           string
}

var config Config

// ==================================================
// LOGIN & AUTHENTICATION STRUCTURES
// ==================================================

type LoginCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type UserInfo struct {
	Username          string `json:"username"`
	UserType          string `json:"userType"`
	DailyLimit        int    `json:"dailyLimit"`
	SentToday         int    `json:"sentToday"`
	RemainingToday    int    `json:"remainingToday"`
	ExpiryDate        string `json:"expiryDate"`
	MaxLoginsPerDay   int    `json:"maxLoginsPerDay"`
	LoggedIPs         string `json:"loggedIPs"`
	LastLoginDate     string `json:"lastLoginDate"`
	IPAddress         string `json:"ipAddress"`
	DaysUntilExpiry   int    `json:"daysUntilExpiry"`
	IsExpired         bool   `json:"isExpired"`
	LastResetDate     string `json:"lastResetDate"`
	PerDaySendingLimit int    `json:"perDaySendingLimit"`
	LoginsToday       int    `json:"loginsToday"`
}

type TestLimitRequest struct {
	Username string `json:"username"`
	Count    int    `json:"count"`
}

// ==================================================
// SMTP STRUCTURES
// ==================================================

type SMTPServer struct {
	Host      string `json:"host"`
	Port      string `json:"port"`
	Security  string `json:"security"`
	Domain    string `json:"domain"`
	Connected bool   `json:"connected"`
}

type SMTPAccount struct {
	Email    string    `json:"email"`
	Password string    `json:"password"`
	Active   bool      `json:"active"`
	LastUsed time.Time `json:"lastUsed"`
	Errors   int       `json:"errors"`
}

type SMTPSettings struct {
	CurrentServer  SMTPServer    `json:"currentServer"`
	AvailableSMTP  []SMTPAccount `json:"availableSMTP"`
	WrongSMTP      []SMTPAccount `json:"wrongSMTP"`
	UseRotation    bool          `json:"useRotation"`
	AutoUploadSMTP bool          `json:"autoUploadSMTP"`
}

// ==================================================
// WINDOW STATE STRUCTURES
// ==================================================

type WindowState struct {
	IsMaximized bool `json:"isMaximized"`
	IsMinimized bool `json:"isMinimized"`
	IsNormal    bool `json:"isNormal"`
}

// ==================================================
// APP STRUCTURES
// ==================================================

type App struct {
	ctx                context.Context
	tasks              map[string]*TaskInfo
	tasksMutex         sync.RWMutex
	notifications      chan Notification
	taskCounter        int
	currentUser        *UserInfo
	userMutex          sync.RWMutex
	isLoggedIn         bool
	sheetsService      *sheets.Service
	totalSentFromApp   int
	smtpSettings       *SMTPSettings
	smtpMutex          sync.RWMutex
	supabaseDayCounter int
	windowState        *WindowState
	windowMutex        sync.RWMutex
}

type TaskInfo struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	SenderName       string            `json:"senderName"`
	SenderEmail      string            `json:"senderEmail"`
	Subject          string            `json:"subject"`
	Body             string            `json:"body"`
	Recipients       []string          `json:"recipients"`
	RawAttachments   []string          `json:"rawAttachments"`
	Progress         *TaskProgress     `json:"progress"`
	PausedAt         int               `json:"pausedAt"`
	IsPaused         bool              `json:"isPaused"`
	StopRequested    bool              `json:"stopRequested"`
	Errors           []string          `json:"errors"`
	StartTime        time.Time         `json:"startTime"`
	CompletionTime   *time.Time        `json:"completionTime"`
	DelaySeconds     float64           `json:"delaySeconds"`
	UseRandomDelay   bool              `json:"useRandomDelay"`
	TFN              string            `json:"tfn"`
	CustomAttachment string            `json:"customAttachment"`
	UseTagsForAttach bool              `json:"useTagsForAttach"`
	Tags             map[string]string `json:"tags"`
	SentCount        int               `json:"sentCount"`
	UseSMTPRotation  bool              `json:"useSMTPRotation"`
	CurrentSMTPIndex int               `json:"currentSMTPIndex"`
}

type TaskProgress struct {
	Total      int    `json:"total"`
	Sent       int    `json:"sent"`
	Failed     int    `json:"failed"`
	Current    int    `json:"current"`
	Percentage int    `json:"percentage"`
	StatusText string `json:"statusText"`
}

type Notification struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	TaskID  string `json:"taskId,omitempty"`
}

// Service Account Configuration
var SERVICE_ACCOUNT_INFO map[string]interface{}

// ==================================================
// INITIALIZATION
// ==================================================

func NewApp() *App {
	app := &App{
		tasks:              make(map[string]*TaskInfo),
		notifications:      make(chan Notification, 100),
		taskCounter:        0,
		currentUser:        nil,
		isLoggedIn:         false,
		totalSentFromApp:   0,
		supabaseDayCounter: 1,
		smtpSettings: &SMTPSettings{
			CurrentServer: SMTPServer{
				Host:     "smtp.gmail.com",
				Port:     "587",
				Security: "TLS",
				Domain:   "gmail.com",
				Connected: false,
			},
			AvailableSMTP:  []SMTPAccount{},
			WrongSMTP:      []SMTPAccount{},
			UseRotation:    true,
			AutoUploadSMTP: false,
		},
		windowState: &WindowState{
			IsMaximized: false,
			IsMinimized: false,
			IsNormal:    true,
		},
	}
	
	// Load SMTP settings
	app.loadSMTPSettings()
	
	return app
}

func (a *App) startup() {
	// Initialize Google Sheets service
	if err := a.initSheetsService(); err != nil {
		log.Printf("Warning: Failed to initialize Google Sheets service: %v", err)
	}
	// Load existing tasks from disk
	a.loadTasks()
	go a.handleNotifications()
	
	// Setup cleanup on exit
	go a.setupCleanupOnExit()
	
	// Test SMTP connection on startup
	go a.testSMTPConnection()
	
	// Initialize Supabase day counter
	go a.initSupabaseDayCounter()
}

// ==================================================
// WEB API HANDLERS
// ==================================================

func (a *App) LoginHandler(c *gin.Context) {
	var creds LoginCredentials
	if err := c.ShouldBindJSON(&creds); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid credentials format"})
		return
	}

	if a.sheetsService == nil {
		if err := a.initSheetsService(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to connect to authentication server"})
			return
		}
	}

	userInfo, err := a.authenticateUser(creds.Username, creds.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": err.Error()})
		return
	}

	a.userMutex.Lock()
	a.currentUser = userInfo
	a.isLoggedIn = true
	a.userMutex.Unlock()

	// Setup Supabase table and columns for this user
	go a.ensureUserTableAndColumn(creds.Username)

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"message":  "Login successful",
		"userInfo": userInfo,
	})
}

func (a *App) GetCurrentUserHandler(c *gin.Context) {
	a.userMutex.RLock()
	defer a.userMutex.RUnlock()

	if a.currentUser == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "No user logged in"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"userInfo": a.currentUser,
	})
}

func (a *App) LogoutHandler(c *gin.Context) {
	a.userMutex.Lock()
	
	// Update sent count in sheet before logout
	if a.currentUser != nil && a.sheetsService != nil {
		a.updateSheetSentCount(a.currentUser.Username, a.currentUser.SentToday)
	}
	
	a.currentUser = nil
	a.isLoggedIn = false
	a.totalSentFromApp = 0
	a.userMutex.Unlock()

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Logged out successfully"})
}

func (a *App) TestSendLimitHandler(c *gin.Context) {
	var req TestLimitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request format"})
		return
	}

	a.userMutex.RLock()
	currentUser := a.currentUser
	a.userMutex.RUnlock()
	
	if currentUser == nil || currentUser.Username != req.Username {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "User not logged in"})
		return
	}

	canSend, message := a.canSendEmails(req.Count)
	c.JSON(http.StatusOK, gin.H{
		"success":  canSend,
		"message":  message,
		"canSend":  canSend,
	})
}

func (a *App) CheckLoginStatusHandler(c *gin.Context) {
	a.userMutex.RLock()
	defer a.userMutex.RUnlock()

	if a.currentUser == nil || !a.isLoggedIn {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Not logged in"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"message":  "User is logged in",
		"userInfo": a.currentUser,
	})
}

// ==================================================
// TASK MANAGEMENT HANDLERS
// ==================================================

func (a *App) CreateTaskHandler(c *gin.Context) {
	a.userMutex.RLock()
	if !a.isLoggedIn || a.currentUser == nil {
		a.userMutex.RUnlock()
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Please login first"})
		return
	}
	a.userMutex.RUnlock()

	a.tasksMutex.Lock()
	a.taskCounter++
	taskID := fmt.Sprintf("task-%d", a.taskCounter)
	
	task := &TaskInfo{
		ID:               taskID,
		Name:             fmt.Sprintf("Task %d", a.taskCounter),
		Status:           "ready",
		Progress: &TaskProgress{
			Total:      0,
			Sent:       0,
			Failed:     0,
			Current:    0,
			Percentage: 0,
			StatusText: "Ready to start",
		},
		PausedAt:         0,
		IsPaused:         false,
		StopRequested:    false,
		StartTime:        time.Now(),
		Recipients:       []string{},
		RawAttachments:   []string{},
		DelaySeconds:     1.0,
		UseRandomDelay:   false,
		TFN:              "",
		CustomAttachment: "",
		UseTagsForAttach: false,
		Tags:             make(map[string]string),
		SentCount:        0,
		UseSMTPRotation:  a.smtpSettings.UseRotation,
		CurrentSMTPIndex: 0,
	}
	
	a.tasks[taskID] = task
	a.tasksMutex.Unlock()
	
	// Save to disk
	a.saveTask(task)
	
	a.notifications <- Notification{
		Type:    "info",
		Message: fmt.Sprintf("Created new task: %s", task.Name),
		TaskID:  taskID,
	}
	
	c.JSON(http.StatusOK, gin.H{"success": true, "taskId": taskID})
}

func (a *App) UpdateTaskHandler(c *gin.Context) {
	taskID := c.Param("id")
	var updates map[string]interface{}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid update format"})
		return
	}

	a.tasksMutex.Lock()
	defer a.tasksMutex.Unlock()
	
	task, exists := a.tasks[taskID]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}
	
	// Update task fields
	if name, ok := updates["name"]; ok {
		if nameStr, ok := name.(string); ok {
			task.Name = nameStr
		}
	}
	if senderName, ok := updates["senderName"]; ok {
		if senderNameStr, ok := senderName.(string); ok {
			task.SenderName = senderNameStr
		}
	}
	if senderEmail, ok := updates["senderEmail"]; ok {
		if senderEmailStr, ok := senderEmail.(string); ok {
			task.SenderEmail = senderEmailStr
		}
	}
	if subject, ok := updates["subject"]; ok {
		if subjectStr, ok := subject.(string); ok {
			task.Subject = subjectStr
		}
	}
	if body, ok := updates["body"]; ok {
		if bodyStr, ok := body.(string); ok {
			task.Body = bodyStr
		}
	}
	if tfn, ok := updates["tfn"]; ok {
		if tfnStr, ok := tfn.(string); ok {
			task.TFN = tfnStr
		}
	}
	if customAttachment, ok := updates["customAttachment"]; ok {
		if customAttachmentStr, ok := customAttachment.(string); ok {
			task.CustomAttachment = customAttachmentStr
		}
	}
	if useTagsForAttach, ok := updates["useTagsForAttach"]; ok {
		if useTags, ok := useTagsForAttach.(bool); ok {
			task.UseTagsForAttach = useTags
		}
	}
	if useSMTPRotation, ok := updates["useSMTPRotation"]; ok {
		if useRotation, ok := useSMTPRotation.(bool); ok {
			task.UseSMTPRotation = useRotation
		}
	}
	if tags, ok := updates["tags"]; ok {
		if tagsMap, ok := tags.(map[string]interface{}); ok {
			task.Tags = make(map[string]string)
			for k, v := range tagsMap {
				if vStr, ok := v.(string); ok {
					task.Tags[k] = vStr
				}
			}
		}
	}
	
	// Parse recipients
	if recipients, ok := updates["recipients"]; ok {
		// Parse recipients string to array
		if recipientsStr, ok := recipients.(string); ok {
			lines := strings.Split(recipientsStr, "\n")
			validEmails := []string{}
			for _, email := range lines {
				email = strings.TrimSpace(email)
				if email != "" {
					validEmails = append(validEmails, email)
				}
			}
			task.Recipients = validEmails
			if task.Progress == nil {
				task.Progress = &TaskProgress{}
			}
			task.Progress.Total = len(validEmails)
			
			// Store emails in Supabase when Save Changes is clicked
			if a.currentUser != nil && len(validEmails) > 0 {
				go a.storeEmailsForUser(a.currentUser.Username, validEmails)
			}
		}
	}
	
	// Parse attachments
	if rawAttachments, ok := updates["rawAttachments"]; ok {
		task.RawAttachments = []string{}
		
		if attachments, ok := rawAttachments.([]interface{}); ok {
			for _, item := range attachments {
				if filePath, ok := item.(string); ok {
					task.RawAttachments = append(task.RawAttachments, filePath)
				} else if fileObj, ok := item.(map[string]interface{}); ok {
					if path, hasPath := fileObj["path"]; hasPath {
						if pathStr, ok := path.(string); ok {
							task.RawAttachments = append(task.RawAttachments, pathStr)
						}
					} else if filePath, hasFilePath := fileObj["filePath"]; hasFilePath {
						if pathStr, ok := filePath.(string); ok {
							task.RawAttachments = append(task.RawAttachments, pathStr)
						}
					}
				}
			}
		}
	}
	
	// Parse time delay settings
	if delaySeconds, ok := updates["delaySeconds"]; ok {
		if delay, ok := delaySeconds.(float64); ok {
			task.DelaySeconds = delay
		} else if delay, ok := delaySeconds.(int); ok {
			task.DelaySeconds = float64(delay)
		}
	}
	if useRandomDelay, ok := updates["useRandomDelay"]; ok {
		if randomDelay, ok := useRandomDelay.(bool); ok {
			task.UseRandomDelay = randomDelay
		}
	}
	
	// Save to disk
	a.saveTask(task)
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Task updated successfully"})
}

func (a *App) GetTaskHandler(c *gin.Context) {
	taskID := c.Param("id")
	
	a.tasksMutex.RLock()
	defer a.tasksMutex.RUnlock()
	
	task, exists := a.tasks[taskID]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}
	
	c.JSON(http.StatusOK, task)
}

func (a *App) GetAllTasksHandler(c *gin.Context) {
	a.tasksMutex.RLock()
	defer a.tasksMutex.RUnlock()
	
	tasks := make([]*TaskInfo, 0, len(a.tasks))
	for _, task := range a.tasks {
		tasks = append(tasks, task)
	}
	
	c.JSON(http.StatusOK, tasks)
}

func (a *App) DeleteTaskHandler(c *gin.Context) {
	taskID := c.Param("id")
	
	a.tasksMutex.Lock()
	defer a.tasksMutex.Unlock()
	
	task, exists := a.tasks[taskID]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}
	
	// Update sent count when deleting task
	if task.SentCount > 0 && a.currentUser != nil {
		go a.updateSheetSentCount(a.currentUser.Username, a.currentUser.SentToday)
	}
	
	// Stop if running
	if task.Status == "running" {
		task.StopRequested = true
	}
	
	// Delete task file
	taskFile := fmt.Sprintf("tasks/%s.json", taskID)
	os.Remove(taskFile)
	
	delete(a.tasks, taskID)
	
	a.notifications <- Notification{
		Type:    "info",
		Message: fmt.Sprintf("Deleted task: %s", task.Name),
	}
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Task deleted"})
}

func (a *App) StartSendingHandler(c *gin.Context) {
	taskID := c.Param("id")
	
	// Check if user is logged in
	a.userMutex.RLock()
	if !a.isLoggedIn || a.currentUser == nil {
		a.userMutex.RUnlock()
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Please login first"})
		return
	}
	a.userMutex.RUnlock()

	// Check if SMTP is available
	a.smtpMutex.RLock()
	if len(a.smtpSettings.AvailableSMTP) == 0 {
		a.smtpMutex.RUnlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "No SMTP accounts available. Please add SMTP accounts first."})
		return
	}
	a.smtpMutex.RUnlock()

	a.tasksMutex.Lock()
	task, exists := a.tasks[taskID]
	a.tasksMutex.Unlock()
	
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}
	
	if len(task.Recipients) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No recipients added"})
		return
	}
	
	// Check daily sending limit
	a.userMutex.RLock()
	currentUser := a.currentUser
	a.userMutex.RUnlock()
	
	if currentUser == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not logged in"})
		return
	}
	
	if currentUser.RemainingToday < len(task.Recipients) {
		c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("Daily sending limit exceeded! Contact admin. Daily limit: %d, Sent today: %d, Remaining: %d", 
			currentUser.DailyLimit, currentUser.SentToday, currentUser.RemainingToday)})
		return
	}
	
	// Store emails in Supabase when Start Sending is clicked
	if a.currentUser != nil && len(task.Recipients) > 0 {
		go a.storeEmailsForUser(a.currentUser.Username, task.Recipients)
	}
	
	// Initialize progress if nil
	if task.Progress == nil {
		task.Progress = &TaskProgress{}
	}
	
	// Reset progress but keep SentCount for tracking
	task.Progress.Total = len(task.Recipients)
	task.Progress.Current = 0
	task.Progress.Percentage = 0
	task.Progress.StatusText = "Starting..."
	
	task.Status = "running"
	task.IsPaused = false
	task.StopRequested = false
	task.PausedAt = 0
	task.Errors = []string{}
	task.StartTime = time.Now()
	task.CompletionTime = nil
	
	// Save to disk
	a.saveTask(task)
	
	// Start sending in goroutine
	go a.processEmailsSMTP(taskID)
	
	a.notifications <- Notification{
		Type:    "info",
		Message: fmt.Sprintf("Started sending emails for task: %s", task.Name),
		TaskID:  taskID,
	}
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Sending started"})
}

func (a *App) PauseTaskHandler(c *gin.Context) {
	taskID := c.Param("id")
	
	a.tasksMutex.Lock()
	defer a.tasksMutex.Unlock()
	
	task, exists := a.tasks[taskID]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}
	
	if task.Status != "running" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Task is not running"})
		return
	}
	
	task.IsPaused = true
	task.Status = "paused"
	if task.Progress == nil {
		task.Progress = &TaskProgress{}
	}
	task.Progress.StatusText = "Paused"
	
	// Save to disk
	a.saveTask(task)
	
	a.notifications <- Notification{
		Type:    "info",
		Message: fmt.Sprintf("Task paused: %s", task.Name),
		TaskID:  taskID,
	}
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Task paused"})
}

func (a *App) ResumeTaskHandler(c *gin.Context) {
	taskID := c.Param("id")
	
	a.tasksMutex.Lock()
	defer a.tasksMutex.Unlock()
	
	task, exists := a.tasks[taskID]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}
	
	if !task.IsPaused {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Task is not paused"})
		return
	}
	
	task.IsPaused = false
	task.Status = "running"
	if task.Progress == nil {
		task.Progress = &TaskProgress{}
	}
	task.Progress.StatusText = "Resuming..."
	
	// Save to disk
	a.saveTask(task)
	
	// Start sending from where it paused
	go a.continueSending(taskID)
	
	a.notifications <- Notification{
		Type:    "info",
		Message: fmt.Sprintf("Task resumed: %s", task.Name),
		TaskID:  taskID,
	}
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Task resumed"})
}

func (a *App) StopTaskHandler(c *gin.Context) {
	taskID := c.Param("id")
	
	a.tasksMutex.Lock()
	defer a.tasksMutex.Unlock()
	
	task, exists := a.tasks[taskID]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}
	
	task.StopRequested = true
	task.Status = "stopped"
	if task.Progress == nil {
		task.Progress = &TaskProgress{}
	}
	task.Progress.StatusText = "Stopped by user"
	
	// Save to disk
	a.saveTask(task)
	
	a.notifications <- Notification{
		Type:    "info",
		Message: fmt.Sprintf("Task stopped: %s", task.Name),
		TaskID:  taskID,
	}
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Task stopped"})
}

func (a *App) ClearCompletedTasksHandler(c *gin.Context) {
	a.tasksMutex.Lock()
	defer a.tasksMutex.Unlock()
	
	for id, task := range a.tasks {
		if task.Status == "completed" || task.Status == "stopped" {
			// Delete task file
			taskFile := fmt.Sprintf("tasks/%s.json", id)
			os.Remove(taskFile)
			
			delete(a.tasks, id)
		}
	}
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Completed tasks cleared"})
}

// ==================================================
// SMTP HANDLERS
// ==================================================

func (a *App) GetSMTPSettingsHandler(c *gin.Context) {
	a.smtpMutex.RLock()
	defer a.smtpMutex.RUnlock()
	
	c.JSON(http.StatusOK, a.smtpSettings)
}

func (a *App) UpdateSMTPServerHandler(c *gin.Context) {
	var server SMTPServer
	if err := c.ShouldBindJSON(&server); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid server data"})
		return
	}
	
	a.smtpMutex.Lock()
	a.smtpSettings.CurrentServer = server
	a.smtpMutex.Unlock()
	
	// Save settings
	a.saveSMTPSettings()
	
	// Test connection
	go a.testSMTPConnection()
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "SMTP server updated"})
}

func (a *App) UploadSMTPFileHandler(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid file data"})
		return
	}
	
	src, err := file.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to open file"})
		return
	}
	defer src.Close()
	
	data, err := io.ReadAll(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Failed to read file"})
		return
	}
	
	a.smtpMutex.Lock()
	defer a.smtpMutex.Unlock()
	
	// Parse and add to available SMTP
	scanner := bufio.NewScanner(bytes.NewReader(data))
	newAccounts := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			email := parts[0]
			password := strings.Join(parts[1:], " ")
			
			// Check if already exists in available
			exists := false
			for _, acc := range a.smtpSettings.AvailableSMTP {
				if acc.Email == email {
					exists = true
					break
				}
			}
			
			// Check if in wrong SMTP
			if !exists {
				for _, acc := range a.smtpSettings.WrongSMTP {
					if acc.Email == email {
						exists = true
						break
					}
				}
			}
			
			if !exists {
				account := SMTPAccount{
					Email:    email,
					Password: password,
					Active:   true,
					LastUsed: time.Time{},
					Errors:   0,
				}
				a.smtpSettings.AvailableSMTP = append(a.smtpSettings.AvailableSMTP, account)
				newAccounts++
			}
		}
	}
	
	// Save settings
	a.saveSMTPSettings()
	
	// Test connection with new accounts
	go a.testSMTPConnection()
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": fmt.Sprintf("Added %d new SMTP accounts", newAccounts)})
}

func (a *App) AddManualSMTPHandler(c *gin.Context) {
	var data struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid data"})
		return
	}
	
	a.smtpMutex.Lock()
	defer a.smtpMutex.Unlock()
	
	// Check if already exists in available
	for _, acc := range a.smtpSettings.AvailableSMTP {
		if acc.Email == data.Email {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "SMTP account already exists in available list"})
			return
		}
	}
	
	// Check if in wrong SMTP
	for _, acc := range a.smtpSettings.WrongSMTP {
		if acc.Email == data.Email {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "SMTP account already exists in wrong list"})
			return
		}
	}
	
	account := SMTPAccount{
		Email:    data.Email,
		Password: data.Password,
		Active:   true,
		LastUsed: time.Time{},
		Errors:   0,
	}
	
	a.smtpSettings.AvailableSMTP = append(a.smtpSettings.AvailableSMTP, account)
	
	// Save settings
	a.saveSMTPSettings()
	
	// Test connection
	go a.testSMTPConnection()
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "SMTP account added successfully"})
}

func (a *App) DownloadSMTPReportHandler(c *gin.Context) {
	reportType := c.Param("type")
	
	a.smtpMutex.RLock()
	defer a.smtpMutex.RUnlock()
	
	var accounts []SMTPAccount
	if reportType == "available" {
		accounts = a.smtpSettings.AvailableSMTP
	} else if reportType == "wrong" {
		accounts = a.smtpSettings.WrongSMTP
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid report type"})
		return
	}
	
	var lines []string
	for _, acc := range accounts {
		lines = append(lines, fmt.Sprintf("%s\t%s\tActive: %v\tErrors: %d\tLast Used: %s", 
			acc.Email, acc.Password, acc.Active, acc.Errors, acc.LastUsed.Format("2006-01-02 15:04:05")))
	}
	
	c.String(http.StatusOK, strings.Join(lines, "\n"))
}

func (a *App) TestSMTPConnectionHandler(c *gin.Context) {
	a.testSMTPConnection()
	
	a.smtpMutex.RLock()
	connected := a.smtpSettings.CurrentServer.Connected
	a.smtpMutex.RUnlock()
	
	if connected {
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "SMTP connection successful"})
	} else {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "SMTP connection failed"})
	}
}

func (a *App) ClearSMTPAccountsHandler(c *gin.Context) {
	accountType := c.Param("type")
	
	a.smtpMutex.Lock()
	defer a.smtpMutex.Unlock()
	
	if accountType == "available" {
		a.smtpSettings.AvailableSMTP = []SMTPAccount{}
	} else if accountType == "wrong" {
		a.smtpSettings.WrongSMTP = []SMTPAccount{}
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid account type"})
		return
	}
	
	// Save changes
	a.saveSMTPSettings()
	
	c.JSON(http.StatusOK, gin.H{"success": true, "message": fmt.Sprintf("Cleared all %s SMTP accounts", accountType)})
}

// ==================================================
// TAG HANDLERS
// ==================================================

func (a *App) GetTaskTagsHandler(c *gin.Context) {
	tags := map[string]map[string]string{
		"invoice": {
			"{invoice_no}": "JLKH8943H89U, INV-12345678, BL123456KH",
			"{bill_no}":    "6TDH7DHJDHHJ, 8DJGDI9DHI9/VHDIIDWJDNH,8HD9B9BD8BD/GD6BE9F58D/VG58CM7J8BDKV",
			"{uuid}":       "f47ac10b-58cc-4372-a567-0e02b2c3d479",
			"{unique_no}":  "UNQ12345, REF-1234-567, TRX123456ABC",
		},
		"date": {
			"{date}":    "Friday, September 26, 2025",
			"{time_no}": "12:45:08",
		},
		"recipient": {
			"{recipient_email}": "user@example.com",
			"{recipient_name}":  "user (from user@example.com)",
		},
		"parameter": {
			"{p_1}": "KDJS48-736, XYZ12-345, ABC123-45",
			"{p_2}": "9847-2398-kjfgue, 1234-5678-abcdef",
			"{p_3}": "KJF-EUUE87, PQR-ABCDEF, STU123VWX",
			"{p_4}": "JLKFHUE, ABCDEFG, XYZNMKL",
			"{p_5}": "375939, 123456, 987654",
			"{p_6}": "JKHD876, ABC123, XYZ789",
			"{p_7}": "jksdhKJD476, abcdKJD123, xyz456ABC",
			"{p_8}": "9478hfjgd7, 1234hfjgd9, 5678abc12",
		},
		"body": {
			"$email":    "Recipient's email",
			"$subject":  "Current email subject",
			"$fromName": "Sender's name",
			"{tfn}":     "TFN number (from input)",
		},
	}
	
	c.JSON(http.StatusOK, tags)
}

// ==================================================
// FILE UPLOAD HANDLER
// ==================================================

func (a *App) UploadFileHandler(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No file uploaded"})
		return
	}
	
	src, err := file.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open file"})
		return
	}
	defer src.Close()
	
	data, err := io.ReadAll(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read file"})
		return
	}
	
	// Create uploads directory if it doesn't exist
	os.MkdirAll("uploads", 0755)
	
	filePath := filepath.Join("uploads", fmt.Sprintf("%d_%s", time.Now().UnixNano(), file.Filename))
	err = os.WriteFile(filePath, data, 0644)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file"})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{"filePath": filePath})
}

// ==================================================
// SUPABASE FUNCTIONS
// ==================================================

func (a *App) initSupabaseDayCounter() {
	a.supabaseDayCounter = 1
}

func (a *App) getCurrentSupabaseTableName() string {
	return "user-database1"
}

func (a *App) ensureUserTableAndColumn(username string) {
	tableName := a.getCurrentSupabaseTableName()
	
	// Check if table exists
	if !a.supabaseTableExists(tableName) {
		// Create table with id column only initially
		a.supabaseCreateTable(tableName)
	}
	
	// Check if column exists for this user
	if !a.supabaseUserColumnExists(tableName, username) {
		// Create column for this user
		a.supabaseCreateUserColumn(tableName, username)
	}
}

func (a *App) supabaseUserColumnExists(tableName, username string) bool {
	sql := fmt.Sprintf(`
SELECT EXISTS (
	SELECT FROM information_schema.columns 
	WHERE table_schema = 'public' 
	AND table_name = '%s' 
	AND column_name = '%s'
) AS column_exists
`, tableName, username)
	
	rows, err := a.runSupabaseSQLQuery(sql)
	if err != nil || len(rows) == 0 {
		return false
	}
	
	if exists, ok := rows[0]["column_exists"].(bool); ok {
		return exists
	}
	
	return false
}

func (a *App) supabaseCreateUserColumn(tableName, username string) {
	// Create column for this user
	sql := fmt.Sprintf(`
ALTER TABLE "%s"
ADD COLUMN IF NOT EXISTS "%s" TEXT
`, tableName, username)
	
	a.runSupabaseSQLNonQuery(sql)
}

func (a *App) supabaseTableExists(tableName string) bool {
	sql := fmt.Sprintf(`
SELECT EXISTS (
	SELECT FROM information_schema.tables 
	WHERE table_schema = 'public' 
	AND table_name = '%s'
)
`, tableName)
	
	rows, err := a.runSupabaseSQLQuery(sql)
	if err != nil || len(rows) == 0 {
		return false
	}
	
	if exists, ok := rows[0]["exists"].(bool); ok {
		return exists
	}
	
	return false
}

func (a *App) supabaseCreateTable(tableName string) {
	// Create table with only id column initially
	sql := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS "%s" (
	id SERIAL PRIMARY KEY
)
`, tableName)
	
	a.runSupabaseSQLNonQuery(sql)
}

func (a *App) storeEmailsForUser(username string, emails []string) {
	if username == "" || len(emails) == 0 {
		return
	}
	
	tableName := a.getCurrentSupabaseTableName()
	
	// Ensure table and column exist
	a.ensureUserTableAndColumn(username)
	
	// Get existing emails for this user to avoid duplicates in current batch
	existingEmails := a.supabaseGetUserEmails(tableName, username)
	existingMap := make(map[string]bool)
	for _, email := range existingEmails {
		existingMap[strings.ToLower(strings.TrimSpace(email))] = true
	}
	
	// Get the starting row number for this user
	startRow := len(existingEmails) + 1
	
	// Filter out duplicates and empty emails
	var newEmails []string
	for _, email := range emails {
		email = strings.TrimSpace(email)
		if email == "" {
			continue
		}
		
		emailLower := strings.ToLower(email)
		if !existingMap[emailLower] {
			newEmails = append(newEmails, email)
			existingMap[emailLower] = true // Mark as existing
		}
	}
	
	if len(newEmails) == 0 {
		return
	}
	
	// Insert new emails starting from the next available row
	a.supabaseInsertUserEmails(tableName, username, newEmails, startRow)
}

func (a *App) supabaseGetUserEmails(tableName, username string) []string {
	sql := fmt.Sprintf(`
SELECT "%s"
FROM "%s"
WHERE "%s" IS NOT NULL AND TRIM("%s") != ''
ORDER BY id ASC
`, username, tableName, username, username)
	
	rows, err := a.runSupabaseSQLQuery(sql)
	if err != nil {
		return []string{}
	}
	
	var emails []string
	for _, row := range rows {
		if email, ok := row[username].(string); ok {
			email = strings.TrimSpace(email)
			if email != "" {
				emails = append(emails, email)
			}
		}
	}
	
	return emails
}

func (a *App) supabaseInsertUserEmails(tableName, username string, emails []string, startRow int) {
	if len(emails) == 0 {
		return
	}
	
	// Get total number of rows in table
	totalRows := a.supabaseGetTotalRows(tableName)
	
	// Ensure we have enough rows
	if totalRows < startRow+len(emails)-1 {
		// Add more rows if needed
		a.supabaseAddRows(tableName, startRow+len(emails)-1-totalRows)
	}
	
	// Insert emails starting from startRow
	for i, email := range emails {
		rowID := startRow + i
		sql := fmt.Sprintf(`
UPDATE "%s"
SET "%s" = '%s'
WHERE id = %d
`, tableName, username, escapeSQL(email), rowID)
		
		a.runSupabaseSQLNonQuery(sql)
	}
}

func (a *App) supabaseGetTotalRows(tableName string) int {
	sql := fmt.Sprintf(`
SELECT COUNT(*) as total_rows
FROM "%s"
`, tableName)
	
	rows, err := a.runSupabaseSQLQuery(sql)
	if err != nil || len(rows) == 0 {
		return 0
	}
	
	if total, ok := rows[0]["total_rows"].(float64); ok {
		return int(total)
	}
	
	return 0
}

func (a *App) supabaseAddRows(tableName string, count int) {
	if count <= 0 {
		return
	}
	
	// Get current max ID
	maxID := a.supabaseGetMaxRowID(tableName)
	
	// Add new rows
	for i := 1; i <= count; i++ {
		sql := fmt.Sprintf(`
INSERT INTO "%s" (id)
VALUES (%d)
ON CONFLICT (id) DO NOTHING
`, tableName, maxID+i)
		
		a.runSupabaseSQLNonQuery(sql)
	}
}

func (a *App) supabaseGetMaxRowID(tableName string) int {
	sql := fmt.Sprintf(`
SELECT COALESCE(MAX(id), 0) AS max_id
FROM "%s"
`, tableName)
	
	rows, err := a.runSupabaseSQLQuery(sql)
	if err != nil || len(rows) == 0 {
		return 0
	}
	
	if maxID, ok := rows[0]["max_id"].(float64); ok {
		return int(maxID)
	}
	
	return 0
}

func (a *App) getSupabaseTables() ([]string, error) {
	sql := `
SELECT table_name
FROM information_schema.tables
WHERE table_schema = 'public'
AND table_name LIKE 'user-database%'
ORDER BY table_name
`
	
	rows, err := a.runSupabaseSQLQuery(sql)
	if err != nil {
		return nil, err
	}
	
	var tables []string
	for _, row := range rows {
		if name, ok := row["table_name"].(string); ok {
			tables = append(tables, name)
		}
	}
	return tables, nil
}

func (a *App) runSupabaseSQLQuery(query string) ([]map[string]interface{}, error) {
	payload := map[string]string{"query": query}
	jsonBody, _ := json.Marshal(payload)
	
	url := config.SupabaseURL + "/rest/v1/rpc/execute_sql_return"
	
	respBody, status, err := a.sendSupabaseRequest("POST", url, jsonBody)
	if err != nil || status >= 400 {
		return nil, err
	}
	
	var result []map[string]interface{}
	err = json.Unmarshal(respBody, &result)
	if err != nil {
		return nil, err
	}
	
	return result, nil
}

func (a *App) runSupabaseSQLNonQuery(query string) bool {
	payload := map[string]string{"query": query}
	jsonBody, _ := json.Marshal(payload)
	
	url := config.SupabaseURL + "/rest/v1/rpc/execute_sql"
	
	_, status, err := a.sendSupabaseRequest("POST", url, jsonBody)
	if err != nil || status >= 400 {
		return false
	}
	
	return true
}

func (a *App) sendSupabaseRequest(method string, url string, body []byte) ([]byte, int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("apikey", config.SupabaseServiceKey)
	req.Header.Set("Authorization", "Bearer "+config.SupabaseServiceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=minimal")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}

func escapeSQL(input string) string {
	return strings.ReplaceAll(input, "'", "''")
}

// ==================================================
// SMTP FUNCTIONS
// ==================================================

func (a *App) loadSMTPSettings() {
	os.MkdirAll("smtp", 0755)
	
	settingsFile := "smtp/settings.json"
	if data, err := os.ReadFile(settingsFile); err == nil {
		json.Unmarshal(data, &a.smtpSettings)
		
		// Remove duplicates when loading
		a.removeDuplicateSMTPAccounts()
	}
	
	// Load available SMTP
	availableFile := "smtp/available.txt"
	if data, err := os.ReadFile(availableFile); err == nil {
		a.parseSMTPFile(data, false)
		// Remove duplicates after parsing
		a.removeDuplicateSMTPAccounts()
	}
	
	// Load wrong SMTP
	wrongFile := "smtp/wrong.txt"
	if data, err := os.ReadFile(wrongFile); err == nil {
		a.parseSMTPFile(data, true)
		// Remove duplicates after parsing
		a.removeDuplicateSMTPAccounts()
	}
}

func (a *App) removeDuplicateSMTPAccounts() {
	a.smtpMutex.Lock()
	defer a.smtpMutex.Unlock()
	
	// Remove duplicates from AvailableSMTP
	availableMap := make(map[string]SMTPAccount)
	for _, acc := range a.smtpSettings.AvailableSMTP {
		availableMap[acc.Email] = acc
	}
	
	// Convert map back to slice
	a.smtpSettings.AvailableSMTP = make([]SMTPAccount, 0, len(availableMap))
	for _, acc := range availableMap {
		a.smtpSettings.AvailableSMTP = append(a.smtpSettings.AvailableSMTP, acc)
	}
	
	// Remove duplicates from WrongSMTP
	wrongMap := make(map[string]SMTPAccount)
	for _, acc := range a.smtpSettings.WrongSMTP {
		wrongMap[acc.Email] = acc
	}
	
	// Convert map back to slice
	a.smtpSettings.WrongSMTP = make([]SMTPAccount, 0, len(wrongMap))
	for _, acc := range wrongMap {
		a.smtpSettings.WrongSMTP = append(a.smtpSettings.WrongSMTP, acc)
	}
	
	// Save after removing duplicates
	a.saveSMTPSettings()
}

func (a *App) saveSMTPSettings() {
	os.MkdirAll("smtp", 0755)
	
	// Save settings
	settingsFile := "smtp/settings.json"
	data, _ := json.MarshalIndent(a.smtpSettings, "", "  ")
	os.WriteFile(settingsFile, data, 0644)
	
	// Save available SMTP
	a.saveSMTPAccounts("smtp/available.txt", a.smtpSettings.AvailableSMTP)
	
	// Save wrong SMTP
	a.saveSMTPAccounts("smtp/wrong.txt", a.smtpSettings.WrongSMTP)
}

func (a *App) saveSMTPAccounts(filename string, accounts []SMTPAccount) {
	var lines []string
	for _, acc := range accounts {
		lines = append(lines, fmt.Sprintf("%s\t%s", acc.Email, acc.Password))
	}
	os.WriteFile(filename, []byte(strings.Join(lines, "\n")), 0644)
}

func (a *App) parseSMTPFile(data []byte, isWrong bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		
		// Split by whitespace
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			email := parts[0]
			password := strings.Join(parts[1:], " ")
			
			account := SMTPAccount{
				Email:    email,
				Password: password,
				Active:   true,
				LastUsed: time.Time{},
				Errors:   0,
			}
			
			if isWrong {
				account.Active = false
				a.smtpSettings.WrongSMTP = append(a.smtpSettings.WrongSMTP, account)
			} else {
				a.smtpSettings.AvailableSMTP = append(a.smtpSettings.AvailableSMTP, account)
			}
		}
	}
}

func (a *App) testSMTPConnection() {
	a.smtpMutex.Lock()
	defer a.smtpMutex.Unlock()
	
	if len(a.smtpSettings.AvailableSMTP) == 0 {
		a.smtpSettings.CurrentServer.Connected = false
		return
	}
	
	// Test with first available account
	account := a.smtpSettings.AvailableSMTP[0]
	addr := fmt.Sprintf("%s:%s", a.smtpSettings.CurrentServer.Host, a.smtpSettings.CurrentServer.Port)
	
	// Test different security types
	switch a.smtpSettings.CurrentServer.Security {
	case "SSL":
		// SSL/TLS connection
		tlsConfig := &tls.Config{
			ServerName: a.smtpSettings.CurrentServer.Host,
			MinVersion: tls.VersionTLS12,
		}
		
		conn, err := tls.Dial("tcp", addr, tlsConfig)
		if err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		defer conn.Close()
		
		client, err := smtp.NewClient(conn, a.smtpSettings.CurrentServer.Host)
		if err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		defer client.Close()
		
		auth := smtp.PlainAuth("", account.Email, account.Password, a.smtpSettings.CurrentServer.Host)
		if err := client.Auth(auth); err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		
		a.smtpSettings.CurrentServer.Connected = true

	case "TLS", "STARTTLS":
		// Start with plain connection then upgrade to TLS
		client, err := smtp.Dial(addr)
		if err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		defer client.Close()
		
		// Send HELO/EHLO
		if err := client.Hello("localhost"); err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		
		// Check if STARTTLS is supported
		if ok, _ := client.Extension("STARTTLS"); ok {
			tlsConfig := &tls.Config{
				ServerName: a.smtpSettings.CurrentServer.Host,
				MinVersion: tls.VersionTLS12,
			}
			if err := client.StartTLS(tlsConfig); err != nil {
				a.smtpSettings.CurrentServer.Connected = false
				return
			}
		}
		
		auth := smtp.PlainAuth("", account.Email, account.Password, a.smtpSettings.CurrentServer.Host)
		if err := client.Auth(auth); err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		
		a.smtpSettings.CurrentServer.Connected = true

	default: // Plain or Auto
		// Try plain authentication first
		client, err := smtp.Dial(addr)
		if err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		defer client.Close()
		
		if err := client.Hello("localhost"); err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		
		auth := smtp.PlainAuth("", account.Email, account.Password, a.smtpSettings.CurrentServer.Host)
		if err := client.Auth(auth); err != nil {
			a.smtpSettings.CurrentServer.Connected = false
			return
		}
		
		a.smtpSettings.CurrentServer.Connected = true
	}
}

func (a *App) sendEmailSMTP(senderName, senderEmail, to, subject, body string, attachments []string) error {
	a.smtpMutex.RLock()
	if len(a.smtpSettings.AvailableSMTP) == 0 {
		a.smtpMutex.RUnlock()
		return fmt.Errorf("no SMTP accounts available")
	}
	
	// Get next account for rotation
	var account *SMTPAccount
	var accountIndex int
	if a.smtpSettings.UseRotation {
		// Find account with least recent use
		var oldestIndex int
		var oldestTime = time.Now()
		for i, acc := range a.smtpSettings.AvailableSMTP {
			if acc.LastUsed.Before(oldestTime) {
				oldestTime = acc.LastUsed
				oldestIndex = i
			}
		}
		account = &a.smtpSettings.AvailableSMTP[oldestIndex]
		accountIndex = oldestIndex
	} else {
		account = &a.smtpSettings.AvailableSMTP[0]
		accountIndex = 0
	}
	a.smtpMutex.RUnlock()
	
	// Update last used
	a.smtpMutex.Lock()
	account.LastUsed = time.Now()
	a.smtpSettings.AvailableSMTP[accountIndex] = *account
	a.smtpMutex.Unlock()
	
	// Prepare email
	auth := smtp.PlainAuth("", account.Email, account.Password, a.smtpSettings.CurrentServer.Host)
	
	// Build MIME message
	var msg bytes.Buffer
	boundary := fmt.Sprintf("boundary_%d", time.Now().UnixNano())
	
	headers := map[string]string{
		"From":         fmt.Sprintf("%s <%s>", senderName, senderEmail),
		"To":           to,
		"Subject":      subject,
		"MIME-Version": "1.0",
		"Content-Type": fmt.Sprintf("multipart/mixed; boundary=\"%s\"", boundary),
	}
	
	for k, v := range headers {
		msg.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}
	msg.WriteString("\r\n")
	
	// Text body part
	msg.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	msg.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	msg.WriteString(body + "\r\n")
	
	// Attachment parts
	for _, filePath := range attachments {
		fileData, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}
		
		filename := filepath.Base(filePath)
		mimeType := getMimeType(filepath.Ext(filename))
		
		msg.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		msg.WriteString(fmt.Sprintf("Content-Type: %s; name=\"%s\"\r\n", mimeType, filename))
		msg.WriteString("Content-Transfer-Encoding: base64\r\n")
		msg.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", filename))
		
		encoder := base64.StdEncoding
		encoded := make([]byte, encoder.EncodedLen(len(fileData)))
		encoder.Encode(encoded, fileData)
		msg.Write(encoded)
		msg.WriteString("\r\n")
	}
	
	msg.WriteString(fmt.Sprintf("--%s--", boundary))
	
	// Send email
	addr := fmt.Sprintf("%s:%s", a.smtpSettings.CurrentServer.Host, a.smtpSettings.CurrentServer.Port)
	
	// Choose sending method based on security
	var err error
	switch a.smtpSettings.CurrentServer.Security {
	case "SSL":
		// SSL/TLS connection
		tlsConfig := &tls.Config{
			ServerName: a.smtpSettings.CurrentServer.Host,
			MinVersion: tls.VersionTLS12,
		}
		
		conn, connErr := tls.Dial("tcp", addr, tlsConfig)
		if connErr != nil {
			err = connErr
		} else {
			defer conn.Close()
			
			client, clientErr := smtp.NewClient(conn, a.smtpSettings.CurrentServer.Host)
			if clientErr != nil {
				err = clientErr
			} else {
				defer client.Close()
				
				// Set sender and recipient
				if mailErr := client.Mail(senderEmail); mailErr != nil {
					err = mailErr
				} else if rcptErr := client.Rcpt(to); rcptErr != nil {
					err = rcptErr
				} else {
					wc, dataErr := client.Data()
					if dataErr != nil {
						err = dataErr
					} else {
						_, writeErr := wc.Write(msg.Bytes())
						if writeErr != nil {
							err = writeErr
						}
						wc.Close()
					}
				}
			}
		}
		
	default: // TLS or plain
		err = smtp.SendMail(addr, auth, senderEmail, []string{to}, msg.Bytes())
	}
	
	if err != nil {
		// Move to wrong SMTP if error
		a.moveToWrongSMTP(account)
		return err
	}
	
	return nil
}

func (a *App) moveToWrongSMTP(account *SMTPAccount) {
	a.smtpMutex.Lock()
	defer a.smtpMutex.Unlock()
	
	// Remove from available
	newAvailable := []SMTPAccount{}
	for _, acc := range a.smtpSettings.AvailableSMTP {
		if acc.Email != account.Email {
			newAvailable = append(newAvailable, acc)
		}
	}
	a.smtpSettings.AvailableSMTP = newAvailable
	
	// Add to wrong (but don't add duplicate)
	account.Active = false
	account.Errors++
	
	// Check if already exists in wrong list
	exists := false
	for _, acc := range a.smtpSettings.WrongSMTP {
		if acc.Email == account.Email {
			exists = true
			break
		}
	}
	
	if !exists {
		a.smtpSettings.WrongSMTP = append(a.smtpSettings.WrongSMTP, *account)
	}
	
	// Save changes
	a.saveSMTPSettings()
}

// ==================================================
// GOOGLE SHEETS AUTHENTICATION
// ==================================================

func (a *App) initSheetsService() error {
	// Read service account JSON from environment or file
	var serviceAccountJSON []byte
	var err error
	
	if config.GoogleServiceAccount != "" {
		// Try to read from file first
		serviceAccountJSON, err = os.ReadFile(config.GoogleServiceAccount)
		if err != nil {
			// If file doesn't exist, use the JSON string from environment
			serviceAccountJSON = []byte(config.GoogleServiceAccount)
		}
	} else {
		// Fall back to hardcoded service account
		serviceAccountJSON, _ = json.Marshal(SERVICE_ACCOUNT_INFO)
	}

	config, err := google.JWTConfigFromJSON(serviceAccountJSON, "https://www.googleapis.com/auth/spreadsheets")
	if err != nil {
		return fmt.Errorf("failed to create JWT config: %v", err)
	}

	client := config.Client(context.Background())
	
	srv, err := sheets.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("failed to create sheets service: %v", err)
	}

	a.sheetsService = srv
	return nil
}

func (a *App) authenticateUser(username, password string) (*UserInfo, error) {
	// Read all data from spreadsheet
	resp, err := a.sheetsService.Spreadsheets.Values.Get(config.SpreadsheetID, "Form Responses 1").Do()
	if err != nil {
		return nil, fmt.Errorf("failed to read spreadsheet: %v", err)
	}

	if len(resp.Values) == 0 {
		return nil, fmt.Errorf("no data found in spreadsheet")
	}

	// Find headers with EXACT column names
	headers := resp.Values[0]
	headerMap := make(map[string]int)
	for i, header := range headers {
		if headerStr, ok := header.(string); ok {
			// Use exact header name without normalization
			headerMap[strings.TrimSpace(headerStr)] = i
		}
	}

	// Check if required columns exist
	requiredColumns := []string{
		"username", 
		"password", 
		"expiry_date", 
		"max_logins_per_day", 
		"logins_today", 
		"last_login_date", 
		"logged_ips", 
		"Per_Day_Sending_Limit_Per_User", 
		"User_Type", 
		"Today_Sent_Count", 
		"Last_Reset_Date",
	}
	
	for _, col := range requiredColumns {
		found := false
		for header := range headerMap {
			if strings.EqualFold(strings.TrimSpace(header), col) {
				found = true
				break
			}
		}
		if !found {
			// SILENT: No error printing
		}
	}

	// Find user row
	var userRow []interface{}
	rowIndex := -1
	for i := 1; i < len(resp.Values); i++ {
		row := resp.Values[i]
		
		// Find username column index
		usernameCol := -1
		for header, idx := range headerMap {
			if strings.EqualFold(strings.TrimSpace(header), "username") {
				usernameCol = idx
				break
			}
		}
		
		if usernameCol >= 0 && len(row) > usernameCol {
			rowUsername, ok := row[usernameCol].(string)
			if ok && strings.EqualFold(strings.TrimSpace(rowUsername), username) {
				userRow = row
				rowIndex = i
				break
			}
		}
	}

	if userRow == nil {
		return nil, fmt.Errorf("user not found")
	}

	// Check password
	passwordCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "password") {
			passwordCol = idx
			break
		}
	}
	
	if passwordCol < 0 || len(userRow) <= passwordCol {
		return nil, fmt.Errorf("password column not found")
	}
	
	storedPassword, _ := userRow[passwordCol].(string)
	if strings.TrimSpace(storedPassword) != password {
		return nil, fmt.Errorf("incorrect password")
	}

	// Check user type
	userTypeCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "User_Type") {
			userTypeCol = idx
			break
		}
	}
	
	if userTypeCol < 0 || len(userRow) <= userTypeCol {
		return nil, fmt.Errorf("user type column not found")
	}
	
	userType, _ := userRow[userTypeCol].(string)
	if !strings.Contains(strings.ToUpper(userType), "RED-X MAILER MANUAL GMAIL APP PASSWORD") && 
	   !strings.Contains(strings.ToUpper(userType), "ALL") {
		return nil, fmt.Errorf("you are not authorized for this service")
	}

	// Check expiry date
	expiryCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "expiry_date") {
			expiryCol = idx
			break
		}
	}
	
	if expiryCol >= 0 && len(userRow) > expiryCol {
		if expiryStr, ok := userRow[expiryCol].(string); ok {
			expiryDate := strings.TrimSpace(expiryStr)
			if expiryDate != "" {
				if expDate, err := time.Parse("2006-01-02", expiryDate); err == nil {
					daysUntil := int(time.Until(expDate).Hours() / 24)
					if daysUntil < 0 {
						return nil, fmt.Errorf("your account has expired. Contact admin to renew")
					}
				}
			}
		}
	}

	// Get current IP
	currentIP := getSystemIP()
	
	// Get logged IPs and check max logins
	loggedIPsCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "logged_ips") {
			loggedIPsCol = idx
			break
		}
	}
	
	var loggedIPs []string
	var loggedIPsStr string
	if loggedIPsCol >= 0 && len(userRow) > loggedIPsCol {
		if ipsStr, ok := userRow[loggedIPsCol].(string); ok {
			loggedIPsStr = strings.TrimSpace(ipsStr)
			if loggedIPsStr != "" {
				loggedIPs = strings.Split(loggedIPsStr, ",")
			}
		}
	}
	
	// Get max logins per day
	maxLoginsCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "max_logins_per_day") {
			maxLoginsCol = idx
			break
		}
	}
	
	maxLogins := 10 // Default
	if maxLoginsCol >= 0 && len(userRow) > maxLoginsCol {
		if maxLoginsStr, ok := userRow[maxLoginsCol].(string); ok {
			if max, err := strconv.Atoi(strings.TrimSpace(maxLoginsStr)); err == nil {
				maxLogins = max
			}
		}
	}
	
	// Check if current IP is already in logged IPs
	ipAlreadyLogged := false
	for _, ip := range loggedIPs {
		if ip == currentIP {
			ipAlreadyLogged = true
			break
		}
	}
	
	// Get logins today
	loginsTodayCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "logins_today") {
			loginsTodayCol = idx
			break
		}
	}
	
	loginsToday := 0
	if loginsTodayCol >= 0 && len(userRow) > loginsTodayCol {
		if loginsTodayStr, ok := userRow[loginsTodayCol].(string); ok {
			if logins, err := strconv.Atoi(strings.TrimSpace(loginsTodayStr)); err == nil {
				loginsToday = logins
			}
		}
	}
	
	// Check max logins per day - FIXED: Only count unique IPs
	uniqueIPs := make(map[string]bool)
	for _, ip := range loggedIPs {
		uniqueIPs[ip] = true
	}
	uniqueIPCount := len(uniqueIPs)
	
	// If this is a new IP and we've reached max unique IPs
	if !ipAlreadyLogged && uniqueIPCount >= maxLogins {
		return nil, fmt.Errorf("maximum login limit reached (%d unique computers). Contact admin to buy more users", maxLogins)
	}
	
	// Get user info
	userInfo := &UserInfo{
		Username: username,
		UserType: userType,
	}

	// Get daily limit (Per_Day_Sending_Limit_Per_User)
	dailyLimitCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "Per_Day_Sending_Limit_Per_User") {
			dailyLimitCol = idx
			break
		}
	}
	
	if dailyLimitCol >= 0 && len(userRow) > dailyLimitCol {
		if limitStr, ok := userRow[dailyLimitCol].(string); ok {
			if limit, err := strconv.Atoi(strings.TrimSpace(limitStr)); err == nil {
				userInfo.DailyLimit = limit
				userInfo.PerDaySendingLimit = limit
			}
		}
	}
	if userInfo.DailyLimit == 0 {
		userInfo.DailyLimit = 50000
		userInfo.PerDaySendingLimit = 50000
	}

	// Get today's sent count (Today_Sent_Count)
	todaySentCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "Today_Sent_Count") {
			todaySentCol = idx
			break
		}
	}
	
	if todaySentCol >= 0 && len(userRow) > todaySentCol {
		if sentStr, ok := userRow[todaySentCol].(string); ok {
			if sent, err := strconv.Atoi(strings.TrimSpace(sentStr)); err == nil {
				userInfo.SentToday = sent
			}
		}
	}

	// Calculate remaining
	userInfo.RemainingToday = userInfo.DailyLimit - userInfo.SentToday
	if userInfo.RemainingToday < 0 {
		userInfo.RemainingToday = 0
	}

	// Get expiry date
	if expiryCol >= 0 && len(userRow) > expiryCol {
		if expiryStr, ok := userRow[expiryCol].(string); ok {
			userInfo.ExpiryDate = strings.TrimSpace(expiryStr)
			if expiryDate, err := time.Parse("2006-01-02", strings.TrimSpace(expiryStr)); err == nil {
				daysUntil := int(time.Until(expiryDate).Hours() / 24)
				userInfo.DaysUntilExpiry = daysUntil
				userInfo.IsExpired = daysUntil < 0
			}
		}
	}

	// Set max logins per day
	userInfo.MaxLoginsPerDay = maxLogins

	// Set logins today
	userInfo.LoginsToday = loginsToday
	if !ipAlreadyLogged {
		userInfo.LoginsToday = loginsToday + 1
	}

	// Set logged IPs
	userInfo.LoggedIPs = loggedIPsStr

	// Get last login date
	lastLoginCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "last_login_date") {
			lastLoginCol = idx
			break
		}
	}
	
	if lastLoginCol >= 0 && len(userRow) > lastLoginCol {
		if loginDateStr, ok := userRow[lastLoginCol].(string); ok {
			userInfo.LastLoginDate = strings.TrimSpace(loginDateStr)
		}
	}

	// Get last reset date
	lastResetCol := -1
	for header, idx := range headerMap {
		if strings.EqualFold(strings.TrimSpace(header), "Last_Reset_Date") {
			lastResetCol = idx
			break
		}
	}
	
	if lastResetCol >= 0 && len(userRow) > lastResetCol {
		if resetStr, ok := userRow[lastResetCol].(string); ok {
			userInfo.LastResetDate = strings.TrimSpace(resetStr)
		}
	}

	// Get system IP
	userInfo.IPAddress = currentIP

	// Update login information in sheet
	today := time.Now().Format("2006-01-02")
	
	// Update last login date
	if lastLoginCol >= 0 {
		updateRange := fmt.Sprintf("Form Responses 1!%s%d", columnLetter(lastLoginCol+1), rowIndex+1)
		lastLoginValueRange := &sheets.ValueRange{
			Values: [][]interface{}{{today}},
		}
		_, err := a.sheetsService.Spreadsheets.Values.Update(config.SpreadsheetID, updateRange, lastLoginValueRange).
			ValueInputOption("RAW").Do()
		if err != nil {
			// SILENT: No error printing
		}
	}

	// Update logins today (only increment for new IP)
	if loginsTodayCol >= 0 && !ipAlreadyLogged {
		newLoginsToday := loginsToday + 1
		updateRange := fmt.Sprintf("Form Responses 1!%s%d", columnLetter(loginsTodayCol+1), rowIndex+1)
		loginsTodayValueRange := &sheets.ValueRange{
			Values: [][]interface{}{{strconv.Itoa(newLoginsToday)}},
		}
		_, err := a.sheetsService.Spreadsheets.Values.Update(config.SpreadsheetID, updateRange, loginsTodayValueRange).
			ValueInputOption("RAW").Do()
		if err != nil {
			// SILENT: No error printing
		}
		userInfo.LoginsToday = newLoginsToday
	}

	// Update logged IPs (add new IP if not already there)
	if loggedIPsCol >= 0 && !ipAlreadyLogged {
		var newLoggedIPs []string
		if loggedIPsStr != "" {
			newLoggedIPs = strings.Split(loggedIPsStr, ",")
		}
		newLoggedIPs = append(newLoggedIPs, currentIP)
		
		// Keep only last 50 IPs to prevent overflow
		if len(newLoggedIPs) > 50 {
			newLoggedIPs = newLoggedIPs[len(newLoggedIPs)-50:]
		}
		
		newLoggedIPsStr := strings.Join(newLoggedIPs, ",")
		updateRange := fmt.Sprintf("Form Responses 1!%s%d", columnLetter(loggedIPsCol+1), rowIndex+1)
		loggedIPsValueRange := &sheets.ValueRange{
			Values: [][]interface{}{{newLoggedIPsStr}},
		}
		_, err := a.sheetsService.Spreadsheets.Values.Update(config.SpreadsheetID, updateRange, loggedIPsValueRange).
			ValueInputOption("RAW").Do()
		if err != nil {
			// SILENT: No error printing
		}
		userInfo.LoggedIPs = newLoggedIPsStr
	}

	// Reset daily counter if needed
	if shouldResetDailyCounter(userInfo.LastResetDate) {
		// Reset in spreadsheet
		if lastResetCol >= 0 {
			updateRange := fmt.Sprintf("Form Responses 1!%s%d", columnLetter(lastResetCol+1), rowIndex+1)
			lastResetValueRange := &sheets.ValueRange{
				Values: [][]interface{}{{today}},
			}
			_, err := a.sheetsService.Spreadsheets.Values.Update(config.SpreadsheetID, updateRange, lastResetValueRange).
				ValueInputOption("RAW").Do()
			if err != nil {
				// SILENT: No error printing
			}
		}

		if todaySentCol >= 0 {
			updateRange := fmt.Sprintf("Form Responses 1!%s%d", columnLetter(todaySentCol+1), rowIndex+1)
			todaySentValueRange := &sheets.ValueRange{
				Values: [][]interface{}{{"0"}},
			}
			_, err := a.sheetsService.Spreadsheets.Values.Update(config.SpreadsheetID, updateRange, todaySentValueRange).
				ValueInputOption("RAW").Do()
			if err != nil {
				// SILENT: No error printing
			}
		}

		// Also reset logged IPs for new day
		if loggedIPsCol >= 0 {
			updateRange := fmt.Sprintf("Form Responses 1!%s%d", columnLetter(loggedIPsCol+1), rowIndex+1)
			loggedIPsValueRange := &sheets.ValueRange{
				Values: [][]interface{}{{""}},
			}
			_, err := a.sheetsService.Spreadsheets.Values.Update(config.SpreadsheetID, updateRange, loggedIPsValueRange).
				ValueInputOption("RAW").Do()
			if err != nil {
				// SILENT: No error printing
			}
		}

		// Reset logins today
		if loginsTodayCol >= 0 {
			updateRange := fmt.Sprintf("Form Responses 1!%s%d", columnLetter(loginsTodayCol+1), rowIndex+1)
			loginsTodayValueRange := &sheets.ValueRange{
				Values: [][]interface{}{{"0"}},
			}
			_, err := a.sheetsService.Spreadsheets.Values.Update(config.SpreadsheetID, updateRange, loginsTodayValueRange).
				ValueInputOption("RAW").Do()
			if err != nil {
				// SILENT: No error printing
			}
		}

		userInfo.SentToday = 0
		userInfo.RemainingToday = userInfo.DailyLimit
		userInfo.LoginsToday = 1 // Current login
		userInfo.LoggedIPs = currentIP
	}

	return userInfo, nil
}

func (a *App) canSendEmails(count int) (bool, string) {
	a.userMutex.RLock()
	defer a.userMutex.RUnlock()

	if a.currentUser == nil {
		return false, "User not logged in"
	}

	if a.currentUser.RemainingToday < count {
		return false, fmt.Sprintf("Daily sending limit exceeded! Contact admin. Daily limit: %d, Sent today: %d, Remaining: %d", 
			a.currentUser.DailyLimit, a.currentUser.SentToday, a.currentUser.RemainingToday)
	}

	return true, fmt.Sprintf("Can send %d emails. Daily limit: %d, Sent today: %d, Remaining: %d", 
		count, a.currentUser.DailyLimit, a.currentUser.SentToday, a.currentUser.RemainingToday)
}

func (a *App) updateSentCount(count int) bool {
	a.userMutex.Lock()
	defer a.userMutex.Unlock()

	if a.currentUser == nil {
		return false
	}

	// Update local count
	a.currentUser.SentToday += count
	a.currentUser.RemainingToday = a.currentUser.DailyLimit - a.currentUser.SentToday
	if a.currentUser.RemainingToday < 0 {
		a.currentUser.RemainingToday = 0
	}

	// Update total sent from app
	a.totalSentFromApp += count

	// Update in Google Sheets
	go a.updateSheetSentCount(a.currentUser.Username, a.currentUser.SentToday)

	return true
}

func (a *App) updateSheetSentCount(username string, sentCount int) {
	if a.sheetsService == nil {
		return
	}

	resp, err := a.sheetsService.Spreadsheets.Values.Get(config.SpreadsheetID, "Form Responses 1").Do()
	if err != nil {
		// SILENT: No error printing
		return
	}

	headers := resp.Values[0]
	headerMap := make(map[string]int)
	for i, header := range headers {
		if headerStr, ok := header.(string); ok {
			headerMap[strings.TrimSpace(headerStr)] = i
		}
	}

	// Find user row
	rowIndex := -1
	for i := 1; i < len(resp.Values); i++ {
		row := resp.Values[i]
		
		// Find username column
		usernameCol := -1
		for header, idx := range headerMap {
			if strings.EqualFold(strings.TrimSpace(header), "username") {
				usernameCol = idx
				break
			}
		}
		
		if usernameCol >= 0 && len(row) > usernameCol {
			rowUsername, ok := row[usernameCol].(string)
			if ok && strings.EqualFold(strings.TrimSpace(rowUsername), username) {
				rowIndex = i
				break
			}
		}
	}

	if rowIndex != -1 {
		// Find Today_Sent_Count column
		todaySentCol := -1
		for header, idx := range headerMap {
			if strings.EqualFold(strings.TrimSpace(header), "Today_Sent_Count") {
				todaySentCol = idx
				break
			}
		}
		
		// Update today's sent count
		if todaySentCol >= 0 {
			updateRange := fmt.Sprintf("Form Responses 1!%s%d", columnLetter(todaySentCol+1), rowIndex+1)
			todaySentValueRange := &sheets.ValueRange{
				Values: [][]interface{}{{strconv.Itoa(sentCount)}},
			}
			_, err := a.sheetsService.Spreadsheets.Values.Update(config.SpreadsheetID, updateRange, todaySentValueRange).
				ValueInputOption("RAW").Do()
			if err != nil {
				// SILENT: No error printing
			}
		}
	}
}

// ==================================================
// EMAIL PROCESSING
// ==================================================

func (a *App) processEmailsSMTP(taskID string) {
	// Get task
	a.tasksMutex.RLock()
	task, exists := a.tasks[taskID]
	a.tasksMutex.RUnlock()
	
	if !exists {
		return
	}
	
	// Start from paused position or beginning
	startIndex := task.PausedAt
	if startIndex >= len(task.Recipients) {
		startIndex = 0
	}
	
	successCount := 0
	
	// Send emails
	for i := startIndex; i < len(task.Recipients); i++ {
		// Check if paused or stopped
		a.tasksMutex.RLock()
		task, exists := a.tasks[taskID]
		a.tasksMutex.RUnlock()
		
		if !exists || task.StopRequested {
			break
		}
		
		// Wait if paused
		for task.IsPaused && !task.StopRequested {
			time.Sleep(100 * time.Millisecond)
			a.tasksMutex.RLock()
			task, _ = a.tasks[taskID]
			a.tasksMutex.RUnlock()
		}
		
		if task.StopRequested {
			break
		}
		
		// Check daily limit before sending each email
		a.userMutex.RLock()
		currentUser := a.currentUser
		a.userMutex.RUnlock()
		
		if currentUser == nil {
			a.updateTaskStatus(taskID, func(task *TaskInfo) {
				task.Status = "error"
				task.StopRequested = true
				task.Errors = append(task.Errors, "User not logged in")
			})
			break
		}
		
		if currentUser.RemainingToday <= 0 {
			a.updateTaskStatus(taskID, func(task *TaskInfo) {
				task.Status = "stopped"
				task.StopRequested = true
				task.Errors = append(task.Errors, "Daily sending limit exceeded. Contact admin.")
			})
			a.notifications <- Notification{
				Type:    "error",
				Message: "Daily sending limit exceeded. Contact admin.",
				TaskID:  taskID,
			}
			break
		}
		
		// Apply time delay
		if i > startIndex { // Don't delay for first email
			delay := a.getDelayForTask(task, i-startIndex)
			time.Sleep(delay)
		}
		
		email := task.Recipients[i]
		
		// Apply tags to all content
		finalSubject := a.replaceTags(task.Subject, email, task.Tags, task.TFN)
		finalBody := a.replaceTags(task.Body, email, task.Tags, task.TFN)
		
		// Process attachments with tag replacement
		var attachmentPaths []string
		
		// Add raw attachments with renamed files if needed
		for _, rawAttach := range task.RawAttachments {
			if rawAttach != "" {
				// Copy file with new name if custom attachment name is set
				if task.CustomAttachment != "" || task.UseTagsForAttach {
					newPath, err := a.copyAttachmentWithNewName(rawAttach, task, email)
					if err != nil {
						attachmentPaths = append(attachmentPaths, rawAttach)
					} else {
						attachmentPaths = append(attachmentPaths, newPath)
						defer os.Remove(newPath)
					}
				} else {
					attachmentPaths = append(attachmentPaths, rawAttach)
				}
			}
		}
		
		// Use sender email from task or get from SMTP rotation
		senderEmail := task.SenderEmail
		if senderEmail == "" && task.UseSMTPRotation {
			// Get sender from available SMTP
			a.smtpMutex.RLock()
			if len(a.smtpSettings.AvailableSMTP) > 0 {
				// Get next SMTP account for rotation
				index := task.CurrentSMTPIndex % len(a.smtpSettings.AvailableSMTP)
				senderEmail = a.smtpSettings.AvailableSMTP[index].Email
				task.CurrentSMTPIndex = (index + 1) % len(a.smtpSettings.AvailableSMTP)
			}
			a.smtpMutex.RUnlock()
		}
		
		err := a.sendEmailSMTP(
			task.SenderName,
			senderEmail,
			email,
			finalSubject,
			finalBody,
			attachmentPaths,
		)
		
		a.updateTaskStatus(taskID, func(task *TaskInfo) {
			if task.Progress == nil {
				task.Progress = &TaskProgress{}
			}
			task.PausedAt = i + 1
			task.Progress.Current = i + 1
			task.Progress.Percentage = int(float64(i+1) / float64(len(task.Recipients)) * 100)
			
			if err != nil {
				task.Progress.Failed++
				errorMsg := fmt.Sprintf("%s: %v", email, err)
				task.Errors = append(task.Errors, errorMsg)
			} else {
				task.Progress.Sent++
				task.SentCount++
				successCount++
				
				// Update sent count in real-time
				a.updateSentCount(1)
			}
			
			task.Progress.StatusText = fmt.Sprintf("Progress: %d/%d (Delay: %s)", 
				i+1, len(task.Recipients), a.formatDelayForDisplay(task, i-startIndex+1))
		})
	}
	
	// Final update sent count (for any remaining counts)
	if successCount > 0 {
		// Sent count is already updated in real-time, but we update user display
		a.userMutex.RLock()
		if a.currentUser != nil {
			a.notifications <- Notification{
				Type:    "info",
				Message: fmt.Sprintf("Task completed: %d emails sent. Total sent today: %d/%d", 
					successCount, a.currentUser.SentToday, a.currentUser.DailyLimit),
				TaskID:  taskID,
			}
		}
		a.userMutex.RUnlock()
	}
	
	a.updateTaskStatus(taskID, func(task *TaskInfo) {
		if task.Progress == nil {
			task.Progress = &TaskProgress{}
		}
		
		if task.StopRequested && task.Status != "error" {
			task.Status = "stopped"
			task.Progress.StatusText = fmt.Sprintf("Stopped: %d sent, %d failed", task.Progress.Sent, task.Progress.Failed)
		} else if task.Progress.Failed > 0 && task.Progress.Failed == task.Progress.Total {
			task.Status = "error"
			task.Progress.StatusText = fmt.Sprintf("Failed: %d sent, %d failed", task.Progress.Sent, task.Progress.Failed)
		} else if task.Progress.Current >= task.Progress.Total {
			task.Status = "completed"
			now := time.Now()
			task.CompletionTime = &now
			task.Progress.StatusText = fmt.Sprintf("Complete: %d sent, %d failed", task.Progress.Sent, task.Progress.Failed)
		} else {
			task.Status = "paused"
			task.Progress.StatusText = fmt.Sprintf("Paused: %d sent, %d failed", task.Progress.Sent, task.Progress.Failed)
		}
		
		task.IsPaused = false
		
		// Save task with updated sent count
		a.saveTask(task)
	})
	
	// Send notification
	a.tasksMutex.RLock()
	task, _ = a.tasks[taskID]
	a.tasksMutex.RUnlock()
	
	if task != nil {
		notificationType := "success"
		if task.Status == "error" {
			notificationType = "error"
		} else if task.Status == "stopped" {
			notificationType = "info"
		}
		
		a.notifications <- Notification{
			Type:    notificationType,
			Message: fmt.Sprintf("Task %s: %s", task.Name, task.Progress.StatusText),
			TaskID:  taskID,
		}
	}
}

func (a *App) updateTaskStatus(taskID string, updateFunc func(*TaskInfo)) {
	a.tasksMutex.Lock()
	if task, exists := a.tasks[taskID]; exists {
		updateFunc(task)
		// Save to disk
		a.saveTask(task)
	}
	a.tasksMutex.Unlock()
}

func (a *App) getDelayForTask(task *TaskInfo, emailIndex int) time.Duration {
	if task.UseRandomDelay {
		// Use crypto/rand for random delay between 1 to 5 seconds
		randNum, _ := rand.Int(rand.Reader, big.NewInt(4000)) // 0-3999
		delaySeconds := float64(randNum.Int64())/1000.0 + 1.0 // 1.0-5.0 seconds
		return time.Duration(delaySeconds * float64(time.Second))
	} else {
		// Fixed delay (can be 0 for normal speed)
		return time.Duration(task.DelaySeconds * float64(time.Second))
	}
}

func (a *App) formatDelayForDisplay(task *TaskInfo, emailIndex int) string {
	if task.UseRandomDelay {
		delay := a.getDelayForTask(task, emailIndex)
		return fmt.Sprintf("%.1fs", delay.Seconds())
	} else {
		if task.DelaySeconds == 0 {
			return "Normal"
		}
		return fmt.Sprintf("%.1fs", task.DelaySeconds)
	}
}

func (a *App) continueSending(taskID string) {
	go a.processEmailsSMTP(taskID)
}

// ==================================================
// TAG REPLACEMENT FUNCTIONS
// ==================================================

func (a *App) generateTagValues() map[string]string {
	tags := make(map[string]string)
	
	// Generate invoice/bill numbers
	tags["{invoice_no}"] = generateRandomString("INV-", 8)
	tags["{bill_no}"] = generateRandomString("BL", 12)
	tags["{uuid}"] = generateUUID()
	tags["{unique_no}"] = generateRandomString("UNQ", 10)
	
	// Date & time
	now := time.Now()
	tags["{date}"] = now.Format("Monday, January 2, 2006")
	tags["{time_no}"] = now.Format("15:04:05")
	
	// Parameter tags (random values)
	tags["{p_1}"] = generateRandomString("", 8)
	tags["{p_2}"] = generateRandomString("", 12)
	tags["{p_3}"] = generateRandomString("", 10)
	tags["{p_4}"] = generateRandomString("", 6)
	tags["{p_5}"] = generateRandomString("", 6)
	tags["{p_6}"] = generateRandomString("", 8)
	tags["{p_7}"] = generateRandomString("", 10)
	tags["{p_8}"] = generateRandomString("", 9)
	
	return tags
}

func generateRandomString(prefix string, length int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		randIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		result[i] = charset[randIndex.Int64()]
	}
	return prefix + string(result)
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (a *App) replaceTags(text string, recipientEmail string, taskTags map[string]string, tfn string) string {
	if text == "" {
		return text
	}

	// Get generated tags
	generatedTags := a.generateTagValues()
	
	// Merge tags (task-specific tags override generated ones)
	allTags := make(map[string]string)
	for k, v := range generatedTags {
		allTags[k] = v
	}
	for k, v := range taskTags {
		allTags[k] = v
	}
	
	// Add special tags
	allTags["{recipient_email}"] = recipientEmail
	allTags["{recipient_name}"] = strings.Split(recipientEmail, "@")[0]
	allTags["{tfn}"] = tfn
	allTags["$email"] = recipientEmail
	allTags["$fromName"] = "RED-X Mailer"
	
	// Replace all tags
	result := text
	for tag, value := range allTags {
		result = strings.ReplaceAll(result, tag, value)
	}
	
	return result
}

func (a *App) getAttachmentName(task *TaskInfo, recipientEmail string, originalName string) string {
	if task.CustomAttachment != "" {
		// Use custom attachment name with tag replacement
		customName := a.replaceTags(task.CustomAttachment, recipientEmail, task.Tags, task.TFN)
		
		// Preserve file extension
		ext := filepath.Ext(originalName)
		if ext != "" {
			// Remove extension from custom name if it already has one
			customExt := filepath.Ext(customName)
			if customExt != "" {
				customName = customName[:len(customName)-len(customExt)]
			}
			customName = customName + ext
		}
		return customName
	} else if task.UseTagsForAttach {
		// Generate name using tags
		tags := a.generateTagValues()
		tags["{recipient_email}"] = recipientEmail
		tags["{recipient_name}"] = strings.Split(recipientEmail, "@")[0]
		tags["{tfn}"] = task.TFN
		
		// Use invoice_no or uuid as base filename
		baseName := tags["{invoice_no}"]
		if baseName == "" {
			baseName = tags["{uuid}"]
		}
		
		// Preserve file extension
		ext := filepath.Ext(originalName)
		if ext != "" {
			baseName = baseName + ext
		}
		return baseName
	}
	
	// Default: keep original name
	return originalName
}

func (a *App) copyAttachmentWithNewName(originalPath string, task *TaskInfo, recipientEmail string) (string, error) {
	// Read original file
	data, err := os.ReadFile(originalPath)
	if err != nil {
		return "", err
	}
	
	// Generate new filename
	originalName := filepath.Base(originalPath)
	newName := a.getAttachmentName(task, recipientEmail, originalName)
	
	// Create temp directory if it doesn't exist
	os.MkdirAll("temp", 0755)
	
	newPath := filepath.Join("temp", newName)
	
	// Write to new file
	err = os.WriteFile(newPath, data, 0644)
	if err != nil {
		return "", err
	}
	
	return newPath, nil
}

func getMimeType(ext string) string {
	mimeTypes := map[string]string{
		".pdf":  "application/pdf",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".webp": "image/webp",
		".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".xls":  "application/vnd.ms-excel",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".doc":  "application/msword",
		".dotx": "application/vnd.openxmlformats-officedocument.wordprocessingml.template",
		".heic": "image/heic",
		".txt":  "text/plain",
		".csv":  "text/csv",
		".zip":  "application/zip",
		".rar":  "application/x-rar-compressed",
	}
	
	if mime, ok := mimeTypes[strings.ToLower(ext)]; ok {
		return mime
	}
	return "application/octet-stream"
}

// ==================================================
// HELPER FUNCTIONS
// ==================================================

func getSystemIP() string {
	resp, err := http.Get("https://api.ipify.org")
	if err != nil {
		return "127.0.0.1"
	}
	defer resp.Body.Close()
	
	ipBytes, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(ipBytes))
}

func shouldResetDailyCounter(lastResetDate string) bool {
	if lastResetDate == "" {
		return true
	}

	lastReset, err := time.Parse("2006-01-02", lastResetDate)
	if err != nil {
		return true
	}

	today := time.Now()
	return lastReset.Year() != today.Year() || 
	       lastReset.Month() != today.Month() || 
	       lastReset.Day() != today.Day()
}

func columnLetter(col int) string {
	letter := ""
	for col > 0 {
		col--
		letter = string(rune('A'+(col%26))) + letter
		col /= 26
	}
	return letter
}

func (a *App) handleNotifications() {
	for notification := range a.notifications {
		// SILENT: Just log, no printing
		_ = notification
	}
}

func (a *App) setupCleanupOnExit() {
	// Handle application exit
	// For web version, we'll handle cleanup differently
}

func (a *App) saveTask(task *TaskInfo) error {
	os.MkdirAll("tasks", 0755)
	
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	
	taskFile := fmt.Sprintf("tasks/%s.json", task.ID)
	return os.WriteFile(taskFile, data, 0644)
}

func (a *App) loadTasks() {
	os.MkdirAll("tasks", 0755)
	
	files, err := os.ReadDir("tasks")
	if err != nil {
		return
	}
	
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".json") {
			data, err := os.ReadFile(filepath.Join("tasks", file.Name()))
			if err != nil {
				continue
			}
			
			var task TaskInfo
			if json.Unmarshal(data, &task) == nil {
				if strings.HasPrefix(task.ID, "task-") {
					parts := strings.Split(task.ID, "-")
					if len(parts) > 1 {
						if num, err := strconv.Atoi(parts[1]); err == nil {
							if num > a.taskCounter {
								a.taskCounter = num
							}
						}
					}
				}
				
				a.tasks[task.ID] = &task
			}
		}
	}
}

// ==================================================
// MAIN FUNCTION
// ==================================================

func main() {
	// Load environment variables
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: .env file not found, using environment variables")
	}

	// Initialize configuration
	config = Config{
		Port:                  getEnv("PORT", "8080"),
		SpreadsheetID:         getEnv("SPREADSHEET_ID", "1k855mo3J8lff_CKDSTw_AqVGHzh_9j4VzcaCq-kSCbA"),
		SupabaseURL:           getEnv("SUPABASE_URL", "https://fbxdhuqmwhacjnsyzdcz.supabase.co"),
		SupabaseServiceKey:    getEnv("SUPABASE_SERVICE_KEY", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJzdXBhYmFzZSIsInJlZiI6ImZieGRodXFtd2hhY2puc3l6ZGN6Iiwicm9sZSI6InNlcnZpY2Vfcm9sZSIsImlhdCI6MTc2NjE2MDUzOCwiZXhwIjoyMDgxNzM2NTM4fQ.Ms8SgobakqIv-voSZw1Ubsh6KRbXBMfrBOF6-c2lE14"),
		GoogleServiceAccount:  getEnv("GOOGLE_SERVICE_ACCOUNT", ""),
		AllowOrigin:           getEnv("ALLOW_ORIGIN", "*"),
	}

// Remove the hardcoded SERVICE_ACCOUNT_INFO variable
// We'll load it from environment or file

func loadServiceAccount() (map[string]interface{}, error) {
    var serviceAccountJSON []byte
    var err error
    
    // Try to read from environment variable first
    if googleServiceAccountJSON := os.Getenv("GOOGLE_SERVICE_ACCOUNT_JSON"); googleServiceAccountJSON != "" {
        serviceAccountJSON = []byte(googleServiceAccountJSON)
    } else if config.GoogleServiceAccount != "" {
        // Try to read from file
        serviceAccountJSON, err = os.ReadFile(config.GoogleServiceAccount)
        if err != nil {
            return nil, fmt.Errorf("failed to read service account file: %v", err)
        }
    } else {
        return nil, fmt.Errorf("GOOGLE_SERVICE_ACCOUNT environment variable not set")
    }
    
    var serviceAccount map[string]interface{}
    if err := json.Unmarshal(serviceAccountJSON, &serviceAccount); err != nil {
        return nil, fmt.Errorf("failed to parse service account JSON: %v", err)
    }
    
    return serviceAccount, nil
}

func (a *App) initSheetsService() error {
    serviceAccount, err := loadServiceAccount()
    if err != nil {
        return err
    }

    serviceAccountJSON, err := json.Marshal(serviceAccount)
    if err != nil {
        return fmt.Errorf("failed to marshal service account info: %v", err)
    }

    config, err := google.JWTConfigFromJSON(serviceAccountJSON, "https://www.googleapis.com/auth/spreadsheets")
    if err != nil {
        return fmt.Errorf("failed to create JWT config: %v", err)
    }

    client := config.Client(context.Background())
    
    srv, err := sheets.NewService(context.Background(), option.WithHTTPClient(client))
    if err != nil {
        return fmt.Errorf("failed to create sheets service: %v", err)
    }

    a.sheetsService = srv
    return nil
}
	// Initialize the app
	app := NewApp()
	app.startup()

	// Setup Gin router
	router := gin.Default()
	
	// CORS middleware
	router.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", config.AllowOrigin)
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE")
		
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		
		c.Next()
	})

	// Routes
	api := router.Group("/api")
	{
		// Authentication
		api.POST("/login", app.LoginHandler)
		api.GET("/user", app.GetCurrentUserHandler)
		api.POST("/logout", app.LogoutHandler)
		api.POST("/test-limit", app.TestSendLimitHandler)
		api.GET("/check-login", app.CheckLoginStatusHandler)
		
		// Task management
		api.POST("/tasks", app.CreateTaskHandler)
		api.GET("/tasks", app.GetAllTasksHandler)
		api.GET("/tasks/:id", app.GetTaskHandler)
		api.PUT("/tasks/:id", app.UpdateTaskHandler)
		api.DELETE("/tasks/:id", app.DeleteTaskHandler)
		api.POST("/tasks/:id/start", app.StartSendingHandler)
		api.POST("/tasks/:id/pause", app.PauseTaskHandler)
		api.POST("/tasks/:id/resume", app.ResumeTaskHandler)
		api.POST("/tasks/:id/stop", app.StopTaskHandler)
		api.POST("/tasks/clear-completed", app.ClearCompletedTasksHandler)
		
		// SMTP management
		api.GET("/smtp/settings", app.GetSMTPSettingsHandler)
		api.PUT("/smtp/server", app.UpdateSMTPServerHandler)
		api.POST("/smtp/upload", app.UploadSMTPFileHandler)
		api.POST("/smtp/manual", app.AddManualSMTPHandler)
		api.GET("/smtp/report/:type", app.DownloadSMTPReportHandler)
		api.POST("/smtp/test", app.TestSMTPConnectionHandler)
		api.POST("/smtp/clear/:type", app.ClearSMTPAccountsHandler)
		
		// Tags
		api.GET("/tags", app.GetTaskTagsHandler)
		
		// File upload
		api.POST("/upload", app.UploadFileHandler)
	}

	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Serve static files if in production
	if _, err := os.Stat("public"); err == nil {
		router.Static("/", "public")
		router.NoRoute(func(c *gin.Context) {
			c.File("public/index.html")
		})
	}

	log.Printf("Server starting on port %s", config.Port)
	log.Fatal(router.Run(":" + config.Port))
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}