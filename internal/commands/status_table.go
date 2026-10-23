package commands

import (
	"strings"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// statusRow is one line of the table: how it spells the service, its state, and
// the URL shown ("" for none).
type statusRow struct {
	name, state, url string
}

// statusView is what a status shows, in both of its forms: the table's rows and
// the JSON document's entries, built together so they cannot disagree.
type statusView struct {
	rows     []statusRow
	services []StatusService
	shared   []StatusShared
	// anyRoute reports whether anything has a URL to show once the router
	// serves it: a routed service, a host route, a running shared service
	// with a routed port.
	anyRoute bool
}

// newStatusView lays out the Compose services, then the host routes, then the
// shared services. A URL is on an entry only when serves says the router serves
// this root's URLs and the service runs (a host route: listens).
func newStatusView(statuses []worktree.ServiceStatus, record registry.RegisteredWorktree, routes worktreeRoutes, listening []worktree.RouteState, sharedEntries []SharedStatusEntry, serves bool) statusView {
	view := statusView{services: make([]StatusService, 0, len(statuses)+len(routes.Host))}
	urls := make(map[string]string, len(routes.Services))
	for _, route := range routes.Services {
		urls[route.Service] = route.URL
	}
	for _, entry := range statuses {
		service := StatusService{Service: entry.Service, State: entry.State.JSON(), Detail: entry.Detail, Port: record.Ports[entry.Service]}
		if entry.State == worktree.Exited {
			code := entry.ExitCode
			service.ExitCode = &code
		}
		if url, routed := urls[entry.Service]; routed {
			view.anyRoute = true
			if serves && entry.State == worktree.Running {
				service.URL = url
			}
		}
		state := entry.Text()
		if entry.Detail != "" {
			state += " (" + entry.Detail + ")"
		}
		view.services = append(view.services, service)
		view.rows = append(view.rows, statusRow{name: entry.Service, state: state, url: service.URL})
	}
	for i, route := range routes.Host {
		view.anyRoute = true
		service := StatusService{Service: route.Service, HostRoute: true, State: listening[i].JSON(), Port: record.HostRoutes[i].Port}
		if serves && listening[i] == worktree.Listening {
			service.URL = route.URL
		}
		view.services = append(view.services, service)
		view.rows = append(view.rows, statusRow{name: route.Service + " (host route)", state: listening[i].String(), url: service.URL})
	}
	for _, entry := range sharedEntries {
		running := entry.State == worktree.Running
		if entry.URL != "" && running {
			view.anyRoute = true
		}
		url := ""
		if serves && running {
			url = entry.URL
		}
		state := sharedStateText(entry)
		switch {
		case entry.Detail != "":
			state += " (" + entry.Detail + ")"
		case running && entry.UsedHere && !entry.Connected:
			// A consumer here that the shared container is not attached to:
			// the one thing about a shared service worth a row's room.
			state += " (not connected)"
		}
		listed := entry
		listed.URL = ""
		view.shared = append(view.shared, StatusShared{SharedStatusEntry: listed, URL: url})
		view.rows = append(view.rows, statusRow{name: entry.Name + " (shared)", state: state, url: url})
	}
	return view
}

// formatStatusTable lays rows out under SERVICE, STATE and URL headings, two
// spaces in, a URL being "-" where none is shown. It is nothing for no rows.
// The names come from the branch's own compose file, so every cell is
// printable before its width is measured.
func formatStatusTable(rows []statusRow) []string {
	if len(rows) == 0 {
		return nil
	}
	cells := make([][3]string, 0, len(rows)+1)
	cells = append(cells, [3]string{"SERVICE", "STATE", "URL"})
	for _, row := range rows {
		url := row.url
		if url == "" {
			url = "-"
		}
		cells = append(cells, [3]string{text.Printable(row.name), text.Printable(row.state), text.Printable(url)})
	}
	var widths [2]int
	for _, row := range cells {
		for i := range widths {
			widths[i] = max(widths[i], text.JSLen(row[i]))
		}
	}
	lines := make([]string, len(cells))
	for i, row := range cells {
		lines[i] = strings.TrimRight("  "+text.PadEnd(row[0], widths[0])+"  "+text.PadEnd(row[1], widths[1])+"  "+row[2], " ")
	}
	return lines
}
