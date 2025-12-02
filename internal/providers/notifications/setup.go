// Package notifications provides access to desktop notification history via freedesktop DBus.
package main

import (
	"bytes"
	_ "embed"
	"encoding/gob"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abenz1267/elephant/v2/internal/comm/handlers"
	"github.com/abenz1267/elephant/v2/internal/util"
	"github.com/abenz1267/elephant/v2/pkg/common"
	"github.com/abenz1267/elephant/v2/pkg/pb/pb"
	"github.com/godbus/dbus/v5"
)

var (
	Name       = "notifications"
	NamePretty = "Notifications"
	config     *Config
	history    = make(map[uint32]*Notification)
	mu         sync.RWMutex
	nextID     uint32 = 1
	file              = common.CacheFile("notifications.gob")
	conn       *dbus.Conn
)

//go:embed README.md
var readme string

const (
	dbusInterface = "org.freedesktop.Notifications"
	dbusPath      = "/org/freedesktop/Notifications"
)

// introspectable implements org.freedesktop.DBus.Introspectable
type introspectable struct {
	xml string
}

func (i *introspectable) Introspect() (string, *dbus.Error) {
	return i.xml, nil
}

type Config struct {
	common.Config `koanf:",squash"`
	MaxItems      int  `koanf:"max_items" desc:"max number of notifications to keep in history" default:"100"`
	Persist       bool `koanf:"persist" desc:"persist notifications across restarts" default:"true"`
}

type Notification struct {
	ID            uint32
	AppName       string
	AppIcon       string
	Summary       string
	Body          string
	Actions       []string
	ExpireTimeout int32
	Time          time.Time
	Hints         map[string]interface{}
}

func Setup() {
	start := time.Now()

	config = &Config{
		Config: common.Config{
			Icon:     "preferences-system-notifications",
			MinScore: 30,
		},
		MaxItems: 100,
		Persist:  true,
	}

	common.LoadConfig(Name, config)

	if config.NamePretty != "" {
		NamePretty = config.NamePretty
	}

	if config.Persist {
		loadFromFile()
	}

	go startDBusServer()

	slog.Info(Name, "history", len(history), "time", time.Since(start))
}

func Available() bool {
	// Check if we can connect to the session bus
	c, err := dbus.SessionBus()
	if err != nil {
		slog.Info(Name, "available", "DBus session bus not available")
		return false
	}
	c.Close()
	return true
}

func startDBusServer() {
	var err error
	conn, err = dbus.SessionBus()
	if err != nil {
		slog.Error(Name, "dbus connect", err)
		return
	}

	// Request the notification service name
	reply, err := conn.RequestName(dbusInterface, dbus.NameFlagDoNotQueue)
	if err != nil {
		slog.Error(Name, "dbus request name", err)
		return
	}

	if reply != dbus.RequestNameReplyPrimaryOwner {
		slog.Warn(Name, "dbus", "another notification daemon is running, listening for signals only")
		listenForSignals()
		return
	}

	// Export our notification handler
	err = conn.Export(&notificationServer{}, dbusPath, dbusInterface)
	if err != nil {
		slog.Error(Name, "dbus export", err)
		return
	}

	// Export introspection
	introspect := &introspectable{xml: `
<node>
  <interface name="org.freedesktop.Notifications">
    <method name="GetCapabilities">
      <arg direction="out" type="as" name="capabilities"/>
    </method>
    <method name="Notify">
      <arg direction="in" type="s" name="app_name"/>
      <arg direction="in" type="u" name="replaces_id"/>
      <arg direction="in" type="s" name="app_icon"/>
      <arg direction="in" type="s" name="summary"/>
      <arg direction="in" type="s" name="body"/>
      <arg direction="in" type="as" name="actions"/>
      <arg direction="in" type="a{sv}" name="hints"/>
      <arg direction="in" type="i" name="expire_timeout"/>
      <arg direction="out" type="u" name="id"/>
    </method>
    <method name="CloseNotification">
      <arg direction="in" type="u" name="id"/>
    </method>
    <method name="GetServerInformation">
      <arg direction="out" type="s" name="name"/>
      <arg direction="out" type="s" name="vendor"/>
      <arg direction="out" type="s" name="version"/>
      <arg direction="out" type="s" name="spec_version"/>
    </method>
    <signal name="NotificationClosed">
      <arg type="u" name="id"/>
      <arg type="u" name="reason"/>
    </signal>
    <signal name="ActionInvoked">
      <arg type="u" name="id"/>
      <arg type="s" name="action_key"/>
    </signal>
  </interface>
</node>`}

	err = conn.Export(introspect, dbusPath, "org.freedesktop.DBus.Introspectable")
	if err != nil {
		slog.Error(Name, "dbus export introspect", err)
		return
	}

	slog.Info(Name, "dbus", "notification server started")

	// Block forever
	select {}
}

func listenForSignals() {
	// If we can't be the primary daemon, listen for notification signals
	err := conn.AddMatchSignal(
		dbus.WithMatchInterface(dbusInterface),
	)
	if err != nil {
		slog.Error(Name, "dbus add match", err)
		return
	}

	c := make(chan *dbus.Signal, 10)
	conn.Signal(c)

	for signal := range c {
		if signal.Name == dbusInterface+".Notify" && len(signal.Body) >= 8 {
			handleNotifySignal(signal.Body)
		}
	}
}

func handleNotifySignal(body []interface{}) {
	appName, _ := body[0].(string)
	replacesID, _ := body[1].(uint32)
	appIcon, _ := body[2].(string)
	summary, _ := body[3].(string)
	bodyText, _ := body[4].(string)
	actions, _ := body[5].([]string)
	hints, _ := body[6].(map[string]dbus.Variant)
	expireTimeout, _ := body[7].(int32)

	hintsMap := make(map[string]interface{})
	for k, v := range hints {
		hintsMap[k] = v.Value()
	}

	storeNotification(appName, replacesID, appIcon, summary, bodyText, actions, hintsMap, expireTimeout)
}

type notificationServer struct{}

func (n *notificationServer) GetCapabilities() ([]string, *dbus.Error) {
	return []string{
		"body",
		"body-markup",
		"actions",
		"icon-static",
		"persistence",
	}, nil
}

func (n *notificationServer) Notify(appName string, replacesID uint32, appIcon, summary, body string, actions []string, hints map[string]dbus.Variant, expireTimeout int32) (uint32, *dbus.Error) {
	hintsMap := make(map[string]interface{})
	for k, v := range hints {
		hintsMap[k] = v.Value()
	}

	id := storeNotification(appName, replacesID, appIcon, summary, body, actions, hintsMap, expireTimeout)

	// Forward to a real notification daemon if configured (could be extended)
	// For now, we just store the notification

	return id, nil
}

func (n *notificationServer) CloseNotification(id uint32) *dbus.Error {
	mu.Lock()
	delete(history, id)
	mu.Unlock()

	if config.Persist {
		saveToFile()
	}

	// Emit NotificationClosed signal
	if conn != nil {
		conn.Emit(dbusPath, dbusInterface+".NotificationClosed", id, uint32(3)) // 3 = closed by call
	}

	return nil
}

func (n *notificationServer) GetServerInformation() (string, string, string, string, *dbus.Error) {
	return "Elephant Notifications", "elephant", "1.0", "1.2", nil
}

func storeNotification(appName string, replacesID uint32, appIcon, summary, body string, actions []string, hints map[string]interface{}, expireTimeout int32) uint32 {
	mu.Lock()
	defer mu.Unlock()

	var id uint32
	if replacesID > 0 {
		id = replacesID
	} else {
		id = nextID
		nextID++
	}

	notification := &Notification{
		ID:            id,
		AppName:       appName,
		AppIcon:       appIcon,
		Summary:       summary,
		Body:          body,
		Actions:       actions,
		Hints:         hints,
		ExpireTimeout: expireTimeout,
		Time:          time.Now(),
	}

	history[id] = notification

	// Trim if over limit
	if len(history) > config.MaxItems {
		trimHistory()
	}

	if config.Persist {
		go saveToFile()
	}

	// Notify frontend of new notification
	handlers.ProviderUpdated <- "notifications:new"

	return id
}

func trimHistory() {
	// Find oldest notification
	var oldestID uint32
	oldestTime := time.Now()

	for id, n := range history {
		if n.Time.Before(oldestTime) {
			oldestID = id
			oldestTime = n.Time
		}
	}

	if oldestID > 0 {
		delete(history, oldestID)
	}
}

func loadFromFile() {
	if common.FileExists(file) {
		f, err := os.ReadFile(file)
		if err != nil {
			slog.Error(Name, "load", err)
			return
		}

		decoder := gob.NewDecoder(bytes.NewReader(f))
		err = decoder.Decode(&history)
		if err != nil {
			slog.Error(Name, "decoding", err)
		}

		// Update nextID to be higher than any existing ID
		for id := range history {
			if id >= nextID {
				nextID = id + 1
			}
		}
	}
}

func saveToFile() {
	mu.RLock()
	defer mu.RUnlock()

	var b bytes.Buffer
	encoder := gob.NewEncoder(&b)

	err := encoder.Encode(history)
	if err != nil {
		slog.Error(Name, "encode", err)
		return
	}

	err = os.MkdirAll(filepath.Dir(file), 0o755)
	if err != nil {
		slog.Error(Name, "createdirs", err)
		return
	}

	err = os.WriteFile(file, b.Bytes(), 0o600)
	if err != nil {
		slog.Error(Name, "writefile", err)
	}
}

func PrintDoc() {
	fmt.Println(readme)
	fmt.Println()
	util.PrintConfig(Config{}, Name)
}

const (
	ActionDismiss    = "dismiss"
	ActionDismissAll = "dismiss_all"
	ActionCopy       = "copy"
	ActionCopyBody   = "copy_body"
)

func Activate(single bool, identifier, action string, query string, args string, format uint8, conn net.Conn) {
	if action == "" {
		action = ActionDismiss
	}

	switch action {
	case ActionDismiss:
		id, err := strconv.ParseUint(identifier, 10, 32)
		if err != nil {
			slog.Error(Name, "parse id", err)
			return
		}

		mu.Lock()
		delete(history, uint32(id))
		mu.Unlock()

		if config.Persist {
			saveToFile()
		}
	case ActionDismissAll:
		mu.Lock()
		history = make(map[uint32]*Notification)
		mu.Unlock()

		if config.Persist {
			saveToFile()
		}
	case ActionCopy, ActionCopyBody:
		id, err := strconv.ParseUint(identifier, 10, 32)
		if err != nil {
			slog.Error(Name, "parse id", err)
			return
		}

		mu.RLock()
		n, ok := history[uint32(id)]
		mu.RUnlock()

		if !ok {
			return
		}

		var content string
		if action == ActionCopyBody {
			content = n.Body
		} else {
			content = fmt.Sprintf("%s\n%s", n.Summary, n.Body)
		}

		copyToClipboard(content)
	default:
		slog.Error(Name, "activate", fmt.Sprintf("unknown action: %s", action))
	}
}

func copyToClipboard(content string) {
	// Use wl-copy if available
	cmd := exec.Command("wl-copy")
	cmd.Stdin = strings.NewReader(content)
	err := cmd.Run()
	if err != nil {
		slog.Error(Name, "copy to clipboard", err)
	}
}

func Query(conn net.Conn, query string, _ bool, exact bool, _ uint8) []*pb.QueryResponse_Item {
	mu.RLock()
	defer mu.RUnlock()

	entries := []*pb.QueryResponse_Item{}

	for _, n := range history {
		text := n.Summary
		subtext := n.Body
		if n.AppName != "" {
			subtext = fmt.Sprintf("[%s] %s", n.AppName, n.Body)
		}

		icon := config.Icon
		if n.AppIcon != "" {
			icon = n.AppIcon
		}

		e := &pb.QueryResponse_Item{
			Identifier:  strconv.FormatUint(uint64(n.ID), 10),
			Text:        text,
			Subtext:     subtext,
			Icon:        icon,
			Type:        pb.QueryResponse_REGULAR,
			Actions:     []string{ActionDismiss, ActionCopy, ActionCopyBody},
			Provider:    Name,
			Preview:     fmt.Sprintf("%s\n\n%s\n\nApp: %s\nTime: %s", n.Summary, n.Body, n.AppName, n.Time.Format(time.RFC1123)),
			PreviewType: util.PreviewTypeText,
			Fuzzyinfo: &pb.QueryResponse_Item_FuzzyInfo{
				Field: "text",
			},
		}

		if query != "" {
			// Search in both summary and body
			searchText := fmt.Sprintf("%s %s %s", n.Summary, n.Body, n.AppName)
			score, pos, start := common.FuzzyScore(query, searchText, exact)

			e.Score = score
			e.Fuzzyinfo.Positions = pos
			e.Fuzzyinfo.Start = start

			if e.Score > config.MinScore {
				entries = append(entries, e)
			}
		} else {
			entries = append(entries, e)
		}
	}

	// Sort by time, newest first
	if query == "" {
		slices.SortStableFunc(entries, func(a, b *pb.QueryResponse_Item) int {
			idA, _ := strconv.ParseUint(a.Identifier, 10, 32)
			idB, _ := strconv.ParseUint(b.Identifier, 10, 32)

			nA := history[uint32(idA)]
			nB := history[uint32(idB)]

			if nA == nil || nB == nil {
				return 0
			}

			return nB.Time.Compare(nA.Time)
		})

		for k := range entries {
			entries[k].Score = int32(10000 - k)
		}
	}

	return entries
}

func Icon() string {
	return config.Icon
}

func HideFromProviderlist() bool {
	return config.HideFromProviderlist
}

func State(provider string) *pb.ProviderStateResponse {
	mu.RLock()
	count := len(history)
	mu.RUnlock()

	states := []string{fmt.Sprintf("%d notifications", count)}
	actions := []string{}

	if count > 0 {
		actions = append(actions, ActionDismissAll)
	}

	return &pb.ProviderStateResponse{
		States:  states,
		Actions: actions,
	}
}
