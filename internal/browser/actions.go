// Playwright-style browser actions executed over CDP. The engine's
// browser-use layer sends interaction/browserExecute commands whose shapes
// mirror the desktop in-app browser's API (see the runtime's zL / ign
// discriminated unions): top-level methods (navigate / snapshot / click /
// fill / playwright ...) and, nested under method "playwright", page actions
// (domSnapshot / elementInfo / evaluate / locator ...). Everything here runs
// against the page's CDP WebSocket — Runtime.evaluate for DOM work, Input.*
// for real pointer/key events, Page.* for navigation and screenshots.

package browser

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// refStore keeps snapshot refs (e1, e2, ...) per tab, mapped to the CSS
// selector path captured when the snapshot was taken. Refs go stale after
// navigations, mirroring the desktop behavior ("Take a fresh snapshot").
var refStore = map[string]map[string]string{}

func setRef(tabID, ref, cssPath string) {
	m := refStore[tabID]
	if m == nil {
		m = map[string]string{}
		refStore[tabID] = m
	}
	m[ref] = cssPath
}

func refFor(tabID, ref string) (string, bool) {
	m := refStore[tabID]
	p, ok := m[ref]
	return p, ok
}

// executeAction runs one browserExecute command against the tab the command
// names (or the first page target). This is the full dispatcher behind
// Browser.Execute's method switch.
func (b *Browser) executeAction(command map[string]any) map[string]any {
	method, _ := command["method"].(string)
	tabID, _ := command["tabId"].(string)

	elapsedMs := func(start time.Time) int64 { return time.Since(start).Round(time.Millisecond).Milliseconds() }
	start := time.Now()
	fail := func(code, msg string) map[string]any {
		return map[string]any{"ok": false, "error": map[string]any{"code": code, "message": msg}, "elapsedMs": elapsedMs(start)}
	}
	ws := func() (string, error) { return b.pageWS(tabID) }

	switch method {
	case "back", "forward":
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		res, err := b.cdpCall(wsURL, tabID, "Page.getNavigationHistory", nil)
		if err != nil {
			return fail("execution_error", err.Error())
		}
		idx := num(res["currentIndex"])
		entries, _ := res["entries"].([]any)
		step := -1.0
		if method == "forward" {
			step = 1.0
		}
		target := int(idx) + int(step)
		if target < 0 || target >= len(entries) {
			return fail("execution_error", "no "+method+" entry in history")
		}
		if _, err := b.cdpCall(wsURL, tabID, "Page.navigateToHistoryEntry", map[string]any{"entryId": entries[target].(map[string]any)["id"]}); err != nil {
			return fail("execution_error", err.Error())
		}
		waitNavSettled(b, wsURL, tabID)
		return map[string]any{"ok": true, "state": b.stateFor(tabID), "elapsedMs": elapsedMs(start)}

	case "reload":
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		if _, err := b.cdpCall(wsURL, tabID, "Page.reload", nil); err != nil {
			return fail("execution_error", err.Error())
		}
		waitNavSettled(b, wsURL, tabID)
		return map[string]any{"ok": true, "state": b.stateFor(tabID), "elapsedMs": elapsedMs(start)}

	case "snapshot":
		val, err := b.captureSnapshot(tabID)
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": elapsedMs(start)}

	case "click", "hover":
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		pt, err := b.actionPoint(wsURL, tabID, command)
		if err != nil {
			return fail("execution_error", err.Error())
		}
		button, _ := command["button"].(string)
		if button == "" {
			button = "left"
		}
		dbl, _ := command["doubleClick"].(bool)
		moveType := "mouseMoved"
		if method == "hover" {
			if _, err := b.cdpCall(wsURL, tabID, "Input.dispatchMouseEvent", map[string]any{"type": moveType, "x": pt.x, "y": pt.y, "button": "none"}); err != nil {
				return fail("execution_error", err.Error())
			}
			return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}
		}
		if _, err := b.cdpCall(wsURL, tabID, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": pt.x, "y": pt.y, "button": button, "clickCount": clickCount(dbl)}); err != nil {
			return fail("execution_error", err.Error())
		}
		if _, err := b.cdpCall(wsURL, tabID, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": pt.x, "y": pt.y, "button": button, "clickCount": clickCount(dbl)}); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "fill":
		ref, _ := command["ref"].(string)
		value, _ := command["value"].(string)
		if err := b.fillRef(tabID, ref, value); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "type":
		text, _ := command["text"].(string)
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		if ref, _ := command["ref"].(string); ref != "" {
			if err := b.focusRef(wsURL, tabID, ref); err != nil {
				return fail("execution_error", err.Error())
			}
		}
		if _, err := b.cdpCall(wsURL, tabID, "Input.insertText", map[string]any{"text": text}); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "press", "cuaKeypress":
		var keys []string
		if method == "press" {
			if k, _ := command["key"].(string); k != "" {
				keys = append(keys, k)
			}
		} else {
			raw, _ := command["keys"].([]any)
			for _, k := range raw {
				if s, ok := k.(string); ok {
					keys = append(keys, s)
				}
			}
		}
		if len(keys) == 0 {
			return fail("execution_error", "no key given")
		}
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		if ref, _ := command["ref"].(string); ref != "" {
			if err := b.focusRef(wsURL, tabID, ref); err != nil {
				return fail("execution_error", err.Error())
			}
		}
		for _, k := range keys {
			if err := pressKey(b, wsURL, tabID, k); err != nil {
				return fail("execution_error", err.Error())
			}
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "scroll", "cuaScroll", "domCuaScroll":
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		sx := num(command["scrollX"])
		sy := num(command["scrollY"])
		if method == "scroll" {
			sy = num(command["y"])
			if sy == 0 {
				sy = 600 // a usable default wheel step
			}
		}
		x := num(command["x"])
		y := num(command["y"])
		if x == 0 && y == 0 {
			x, y = 640, 400
		}
		if _, err := b.cdpCall(wsURL, tabID, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y, "deltaX": -sx, "deltaY": -sy,
		}); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "screenshot", "elementScreenshot":
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		params := map[string]any{"format": "png"}
		if full, _ := command["fullPage"].(bool); full {
			params["captureBeyondViewport"] = true
		}
		if clip, ok := command["clip"].(map[string]any); ok {
			params["clip"] = map[string]any{
				"x": num(clip["x"]), "y": num(clip["y"]),
				"width": num(clip["width"]), "height": num(clip["height"]), "scale": 1,
			}
		}
		if ref, _ := command["ref"].(string); ref != "" {
			css, ok := refFor(tabID, ref)
			if !ok {
				return fail("execution_error", "Browser ref '"+ref+"' is stale or unavailable. Take a fresh snapshot before retrying.")
			}
			rect, err := b.evalJS(wsURL, tabID, elementRectJS(css))
			if err != nil {
				return fail("execution_error", err.Error())
			}
			rm, _ := rect.(map[string]any)
			if rm == nil || num(rm["width"]) == 0 {
				return fail("execution_error", "element has no box: "+ref)
			}
			params["clip"] = map[string]any{
				"x": num(rm["x"]), "y": num(rm["y"]),
				"width": num(rm["width"]), "height": num(rm["height"]), "scale": 1,
			}
		}
		res, err := b.cdpCall(wsURL, tabID, "Page.captureScreenshot", params)
		if err != nil {
			return fail("execution_error", err.Error())
		}
		data, _ := res["data"].(string)
		if data == "" {
			return fail("execution_error", "empty screenshot")
		}
		return map[string]any{"ok": true, "image": map[string]any{"base64": data, "mimeType": "image/png"}, "elapsedMs": elapsedMs(start)}

	case "hoverAt", "elementInfo":
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		val, err := b.evalJS(wsURL, tabID, elementInfoJS(num(command["x"]), num(command["y"])))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": elapsedMs(start)}

	case "evaluate":
		expr, _ := command["expression"].(string)
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		val, err := b.evalJS(wsURL, tabID, expr)
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": elapsedMs(start)}

	case "select":
		ref, _ := command["ref"].(string)
		values, _ := command["values"].([]any)
		vals := make([]string, 0, len(values))
		for _, v := range values {
			if s, ok := v.(string); ok {
				vals = append(vals, s)
			}
		}
		if err := b.selectRef(tabID, ref, vals); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "check":
		ref, _ := command["ref"].(string)
		checked := true
		if c, ok := command["checked"].(bool); ok {
			checked = c
		}
		if err := b.checkRef(tabID, ref, checked); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "drag", "cuaDrag":
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		from := point{num(command["fromX"]), num(command["fromY"])}
		to := point{num(command["toX"]), num(command["toY"])}
		if method == "drag" {
			if ref, _ := command["fromRef"].(string); ref != "" {
				if p, err := b.refPoint(wsURL, tabID, ref); err == nil {
					from = p
				}
			}
			if ref, _ := command["toRef"].(string); ref != "" {
				if p, err := b.refPoint(wsURL, tabID, ref); err == nil {
					to = p
				}
			}
		}
		if raw, ok := command["path"].([]any); ok && len(raw) > 0 {
			first, _ := raw[0].(map[string]any)
			from = point{num(first["x"]), num(first["y"])}
			last, _ := raw[len(raw)-1].(map[string]any)
			to = point{num(last["x"]), num(last["y"])}
		}
		for _, ev := range []map[string]any{
			{"type": "mousePressed", "x": from.x, "y": from.y, "button": "left", "clickCount": 1},
			{"type": "mouseMoved", "x": (from.x + to.x) / 2, "y": (from.y + to.y) / 2, "button": "left"},
			{"type": "mouseMoved", "x": to.x, "y": to.y, "button": "left"},
			{"type": "mouseReleased", "x": to.x, "y": to.y, "button": "left", "clickCount": 1},
		} {
			if _, err := b.cdpCall(wsURL, tabID, "Input.dispatchMouseEvent", ev); err != nil {
				return fail("execution_error", err.Error())
			}
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "getDialog":
		return map[string]any{"ok": true, "value": nil, "elapsedMs": elapsedMs(start)}
	case "handleDialog":
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		accept, _ := command["accept"].(bool)
		params := map[string]any{"accept": accept}
		if t, ok := command["promptText"].(string); ok {
			params["promptText"] = t
		}
		if _, err := b.cdpCall(wsURL, tabID, "Page.handleJavaScriptDialog", params); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "waitFor":
		deadline := time.Now().Add(timeoutOrDefault(command, 5*time.Second))
		wsURL, err := ws()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		selector, _ := command["selector"].(string)
		text, _ := command["text"].(string)
		textGone, _ := command["textGone"].(string)
		for time.Now().Before(deadline) {
			val, err := b.evalJS(wsURL, tabID, waitForProbeJS(selector, text, textGone))
			if err == nil {
				if done, _ := val.(bool); done {
					return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		return fail("timeout", "waitFor condition not met")

	case "playwrightWaitForTimeout":
		ms := num(command["timeoutMs"])
		if ms > 30000 {
			ms = 30000
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return map[string]any{"ok": true, "elapsedMs": elapsedMs(start)}

	case "capabilities":
		return map[string]any{"ok": true, "value": []string{
			"navigate", "back", "forward", "reload", "snapshot", "screenshot", "click",
			"fill", "type", "press", "cuaKeypress", "scroll", "cuaScroll", "domCuaScroll",
			"hover", "select", "check", "drag", "cuaDrag", "elementInfo", "evaluate",
			"getDialog", "handleDialog", "waitFor", "playwrightWaitForTimeout", "playwright",
			"newTab", "activateTab", "closeTab", "list", "getState", "setViewportSize",
		}, "elapsedMs": elapsedMs(start)}

	case "playwright":
		action, _ := command["action"].(map[string]any)
		if action == nil {
			return fail("execution_error", "playwright command requires an action")
		}
		return b.playwrightAction(tabID, action, start)
	}

	return nil // not handled by this dispatcher
}

// playwrightAction implements the desktop IAB's managed playwright actions
// (the ign union): domSnapshot, elementInfo, elementScreenshot, evaluate,
// waitForLoadState, waitForURL, locator.
func (b *Browser) playwrightAction(tabID string, action map[string]any, start time.Time) map[string]any {
	name, _ := action["name"].(string)
	fail := func(code, msg string) map[string]any {
		return map[string]any{"ok": false, "error": map[string]any{"code": code, "message": msg}, "elapsedMs": time.Since(start).Milliseconds()}
	}
	wsURL, err := b.pageWS(tabID)
	if err != nil {
		return fail("execution_error", err.Error())
	}

	switch name {
	case "domSnapshot":
		val, err := b.ariaSnapshot(wsURL, tabID)
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}

	case "elementInfo":
		val, err := b.evalJS(wsURL, tabID, elementInfoJS(num(action["x"]), num(action["y"])))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}

	case "elementScreenshot":
		rect, err := b.evalJS(wsURL, tabID, elementAtRectJS(num(action["x"]), num(action["y"])))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		rm, _ := rect.(map[string]any)
		if rm == nil || num(rm["width"]) == 0 {
			return fail("execution_error", "No matching element was found at the requested point")
		}
		res, err := b.cdpCall(wsURL, tabID, "Page.captureScreenshot", map[string]any{
			"format": "png",
			"clip": map[string]any{
				"x": num(rm["x"]), "y": num(rm["y"]),
				"width": num(rm["width"]), "height": num(rm["height"]), "scale": 1,
			},
		})
		if err != nil {
			return fail("execution_error", err.Error())
		}
		data, _ := res["data"].(string)
		return map[string]any{"ok": true, "image": map[string]any{"base64": data, "mimeType": "image/png"}, "elapsedMs": time.Since(start).Milliseconds()}

	case "evaluate":
		expr, _ := action["expression"].(string)
		val, err := b.evalJS(wsURL, tabID, expr)
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}

	case "waitForLoadState":
		want, _ := action["state"].(string)
		if want == "" {
			want = "load"
		}
		deadline := time.Now().Add(timeoutOrDefault(action, 15*time.Second))
		for time.Now().Before(deadline) {
			val, err := b.evalJS(wsURL, tabID, "document.readyState")
			if err == nil {
				if s, _ := val.(string); s == "complete" || (want == "domcontentloaded" && s != "loading") {
					return map[string]any{"ok": true, "elapsedMs": time.Since(start).Milliseconds()}
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fail("timeout", "waitForLoadState "+want+" not reached")

	case "waitForURL":
		url, _ := action["url"].(string)
		deadline := time.Now().Add(timeoutOrDefault(action, 15*time.Second))
		for time.Now().Before(deadline) {
			val, err := b.evalJS(wsURL, tabID, "location.href")
			if err == nil {
				if s, _ := val.(string); strings.Contains(s, url) {
					return map[string]any{"ok": true, "elapsedMs": time.Since(start).Milliseconds()}
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fail("timeout", "waitForURL not reached: "+url)

	case "locator":
		return b.locatorAction(wsURL, tabID, action, start)

	case "waitForEvent", "downloadPath", "fileChooserSetFiles":
		return fail("execution_error", fmt.Sprintf("Playwright action '%s' is unavailable", name))
	}

	return fail("execution_error", "unknown playwright action: "+name)
}

// locatorAction implements the rgn operation set against a CSS selector.
func (b *Browser) locatorAction(wsURL, tabID string, action map[string]any, start time.Time) map[string]any {
	selector, _ := action["selector"].(string)
	operation, _ := action["operation"].(string)
	fail := func(code, msg string) map[string]any {
		return map[string]any{"ok": false, "error": map[string]any{"code": code, "message": msg}, "elapsedMs": time.Since(start).Milliseconds()}
	}
	if selector == "" {
		return fail("execution_error", "locator requires a selector")
	}

	switch operation {
	case "count":
		val, err := b.evalJS(wsURL, tabID, fmt.Sprintf("document.querySelectorAll(%s).length", jsQuote(selector)))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}
	case "textContent":
		val, err := b.evalJS(wsURL, tabID, fmt.Sprintf("(document.querySelector(%s)||{}).textContent ?? null", jsQuote(selector)))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}
	case "innerText":
		val, err := b.evalJS(wsURL, tabID, fmt.Sprintf("(document.querySelector(%s)||{}).innerText ?? null", jsQuote(selector)))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}
	case "allTextContents":
		val, err := b.evalJS(wsURL, tabID, fmt.Sprintf("[...document.querySelectorAll(%s)].map(e=>e.textContent)", jsQuote(selector)))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}
	case "getAttribute":
		attr, _ := action["attribute"].(string)
		val, err := b.evalJS(wsURL, tabID, fmt.Sprintf("(document.querySelector(%s)||{}).getAttribute?.(%s) ?? null", jsQuote(selector), jsQuote(attr)))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}
	case "isVisible":
		val, err := b.evalJS(wsURL, tabID, isVisibleJS(selector))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}
	case "isEnabled":
		val, err := b.evalJS(wsURL, tabID, fmt.Sprintf("!(document.querySelector(%s)?.disabled ?? true)", jsQuote(selector)))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}
	case "click", "dblclick":
		count := 1
		if operation == "dblclick" {
			count = 2
		}
		pt, err := b.selectorPoint(wsURL, tabID, selector)
		if err != nil {
			return fail("execution_error", err.Error())
		}
		for _, ev := range []map[string]any{
			{"type": "mousePressed", "x": pt.x, "y": pt.y, "button": "left", "clickCount": count},
			{"type": "mouseReleased", "x": pt.x, "y": pt.y, "button": "left", "clickCount": count},
		} {
			if _, err := b.cdpCall(wsURL, tabID, "Input.dispatchMouseEvent", ev); err != nil {
				return fail("execution_error", err.Error())
			}
		}
		return map[string]any{"ok": true, "elapsedMs": time.Since(start).Milliseconds()}
	case "fill":
		value, _ := action["value"].(string)
		if err := b.fillSelector(tabID, selector, value); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": time.Since(start).Milliseconds()}
	case "press":
		key, _ := action["key"].(string)
		if err := b.focusSelector(wsURL, tabID, selector); err != nil {
			return fail("execution_error", err.Error())
		}
		if err := pressKey(b, wsURL, tabID, key); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": time.Since(start).Milliseconds()}
	case "selectOption":
		selections, _ := action["selections"].([]any)
		vals := make([]string, 0, len(selections))
		for _, s := range selections {
			sm, _ := s.(map[string]any)
			if sm == nil {
				continue
			}
			if v, ok := sm["value"].(string); ok {
				vals = append(vals, v)
			} else if l, ok := sm["label"].(string); ok {
				vals = append(vals, "label:"+l)
			} else if i := num(sm["index"]); i > 0 {
				vals = append(vals, fmt.Sprintf("index:%d", int(i)))
			}
		}
		if err := b.evalVoid(wsURL, tabID, selectOptionJS(selector, vals)); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": time.Since(start).Milliseconds()}
	case "setChecked":
		checked, _ := action["checked"].(bool)
		want := "true"
		if !checked {
			want = "false"
		}
		if err := b.evalVoid(wsURL, tabID, fmt.Sprintf(
			`(()=>{const e=document.querySelector(%s);if(!e)throw new Error("not found");if(e.checked!==%s)e.click();})()`,
			jsQuote(selector), want)); err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "elapsedMs": time.Since(start).Milliseconds()}
	case "evaluate":
		expr, _ := action["expression"].(string)
		val, err := b.evalJS(wsURL, tabID, locatorEvaluateJS(selector, expr))
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "value": val, "elapsedMs": time.Since(start).Milliseconds()}
	case "waitFor":
		state, _ := action["state"].(string)
		if state == "" {
			state = "visible"
		}
		deadline := time.Now().Add(timeoutOrDefault(action, 5*time.Second))
		for time.Now().Before(deadline) {
			val, err := b.evalJS(wsURL, tabID, locatorStateJS(selector, state))
			if err == nil {
				if done, _ := val.(bool); done {
					return map[string]any{"ok": true, "elapsedMs": time.Since(start).Milliseconds()}
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fail("timeout", "locator waitFor "+state+" not met")
	}

	return fail("execution_error", "unknown locator operation: "+operation)
}

// captureSnapshot mirrors the desktop's managed snapshot: interactive
// elements with refs (e1, e2, ...) plus a structural DOM outline. Refs are
// registered for later click/fill/screenshot-by-ref resolution.
func (b *Browser) captureSnapshot(tabID string) (map[string]any, error) {
	wsURL, err := b.pageWS(tabID)
	if err != nil {
		return nil, err
	}
	effectiveTab := tabID
	if effectiveTab == "" {
		if t := b.tabByID(""); t != nil {
			effectiveTab = t.ID
		}
	}
	val, err := b.evalJS(wsURL, tabID, snapshotJS())
	if err != nil {
		return nil, err
	}
	obj, _ := val.(map[string]any)
	if obj == nil {
		return nil, fmt.Errorf("snapshot returned no data")
	}
	if refs, ok := obj["refSelectors"].(map[string]any); ok {
		for ref, sel := range refs {
			if s, ok := sel.(string); ok {
				setRef(effectiveTab, ref, s)
			}
		}
	}
	delete(obj, "refSelectors")
	return obj, nil
}

// ariaSnapshot renders a Playwright-ariaSnapshot-like YAML view of the page
// and registers the same refs as captureSnapshot.
func (b *Browser) ariaSnapshot(wsURL, tabID string) (string, error) {
	effectiveTab := tabID
	if effectiveTab == "" {
		if t := b.tabByID(""); t != nil {
			effectiveTab = t.ID
		}
	}
	val, err := b.evalJS(wsURL, tabID, ariaSnapshotJS())
	if err != nil {
		return "", err
	}
	obj, _ := val.(map[string]any)
	if obj == nil {
		return "", fmt.Errorf("aria snapshot returned no data")
	}
	if refs, ok := obj["refs"].(map[string]any); ok {
		for ref, sel := range refs {
			if s, ok := sel.(string); ok {
				setRef(effectiveTab, ref, s)
			}
		}
	}
	text, _ := obj["yaml"].(string)
	return text, nil
}

// --- ref/selector helpers -------------------------------------------------

// actionPoint resolves a click/hover target: explicit x/y wins, then ref.
func (b *Browser) actionPoint(wsURL, tabID string, command map[string]any) (point, error) {
	if x := num(command["x"]); x != 0 || num(command["y"]) != 0 {
		return point{x, num(command["y"])}, nil
	}
	if ref, _ := command["ref"].(string); ref != "" {
		return b.refPoint(wsURL, tabID, ref)
	}
	return point{}, fmt.Errorf("Browser action requires a ref or x/y coordinates")
}

func (b *Browser) refPoint(wsURL, tabID, ref string) (point, error) {
	css, ok := refFor(tabID, ref)
	if !ok {
		return point{}, fmt.Errorf("Browser ref '%s' is stale or unavailable. Take a fresh snapshot before retrying.", ref)
	}
	return b.selectorPoint(wsURL, tabID, css)
}

func (b *Browser) selectorPoint(wsURL, tabID, css string) (point, error) {
	rect, err := b.evalJS(wsURL, tabID, elementRectJS(css))
	if err != nil {
		return point{}, err
	}
	rm, _ := rect.(map[string]any)
	if rm == nil || num(rm["width"]) == 0 || num(rm["height"]) == 0 {
		return point{}, fmt.Errorf("element not found or not visible: %s", css)
	}
	return point{num(rm["x"]) + num(rm["width"])/2, num(rm["y"]) + num(rm["height"])/2}, nil
}

func (b *Browser) focusRef(wsURL, tabID, ref string) error {
	css, ok := refFor(tabID, ref)
	if !ok {
		return fmt.Errorf("Browser ref '%s' is stale or unavailable", ref)
	}
	return b.focusSelector(wsURL, tabID, css)
}

func (b *Browser) focusSelector(wsURL, tabID, css string) error {
	return b.evalVoid(wsURL, tabID, fmt.Sprintf(
		`(()=>{const e=document.querySelector(%s);if(!e)throw new Error("not found: element");e.scrollIntoView({block:"center"});e.focus();})()`,
		jsQuote(css)))
}

func (b *Browser) fillRef(tabID, ref, value string) error {
	css, ok := refFor(tabID, ref)
	if !ok {
		return fmt.Errorf("Browser ref '%s' is stale or unavailable. Take a fresh snapshot before retrying.", ref)
	}
	return b.fillSelector(tabID, css, value)
}

func (b *Browser) fillSelector(tabID, css, value string) error {
	wsURL, err := b.pageWS(tabID)
	if err != nil {
		return err
	}
	return b.evalVoid(wsURL, tabID, fillJS(css, value))
}

func (b *Browser) selectRef(tabID, ref string, values []string) error {
	css, ok := refFor(tabID, ref)
	if !ok {
		return fmt.Errorf("Browser ref '%s' is stale or unavailable", ref)
	}
	wsURL, err := b.pageWS(tabID)
	if err != nil {
		return err
	}
	return b.evalVoid(wsURL, tabID, selectOptionJS(css, values))
}

func (b *Browser) checkRef(tabID, ref string, checked bool) error {
	css, ok := refFor(tabID, ref)
	if !ok {
		return fmt.Errorf("Browser ref '%s' is stale or unavailable", ref)
	}
	wsURL, err := b.pageWS(tabID)
	if err != nil {
		return err
	}
	want := "true"
	if !checked {
		want = "false"
	}
	return b.evalVoid(wsURL, tabID, fmt.Sprintf(
		`(()=>{const e=document.querySelector(%s);if(!e)throw new Error("not found");if(e.checked!==%s)e.click();})()`,
		jsQuote(css), want))
}

// stateFor resolves the getState payload for a tab (or the first page).
func (b *Browser) stateFor(tabID string) map[string]any {
	if t := b.tabByID(tabID); t != nil {
		state := map[string]any{"url": t.URL, "title": t.Title, "canGoBack": true, "canGoForward": true}
		return state
	}
	return b.getState()
}

// waitNavSettled gives navigations a moment so tab titles/URLs catch up
// before the result descriptor is built.
func waitNavSettled(b *Browser, wsURL, tabID string) {
	for i := 0; i < 10; i++ {
		if ready, err := b.evalJS(wsURL, tabID, "document.readyState"); err == nil {
			if s, _ := ready.(string); s == "complete" || s == "interactive" {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	b.refreshTabs()
}

// --- small helpers ---------------------------------------------------------

type point struct{ x, y float64 }

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func clickCount(dbl bool) int {
	if dbl {
		return 2
	}
	return 1
}

func timeoutOrDefault(m map[string]any, d time.Duration) time.Duration {
	if ms := num(m["timeoutMs"]); ms > 0 {
		if ms > 60000 {
			ms = 60000
		}
		return time.Duration(ms) * time.Millisecond
	}
	return d
}

// jsQuote encodes a Go string as a double-quoted JavaScript literal.
func jsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// evalJS evaluates an expression and returns its value (returnByValue).
func (b *Browser) evalJS(wsURL, tabID, expr string) (any, error) {
	res, err := b.cdpCall(wsURL, tabID, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, err
	}
	if rm, ok := res["exceptionDetails"].(map[string]any); ok {
		desc, _ := rm["exception"].(map[string]any)["description"].(string)
		if desc == "" {
			desc = fmt.Sprintf("%v", rm["text"])
		}
		return nil, fmt.Errorf("%s", desc)
	}
	return res["result"].(map[string]any)["value"], nil
}

func (b *Browser) evalVoid(wsURL, tabID, expr string) error {
	_, err := b.evalJS(wsURL, tabID, expr)
	return err
}

// keySpec maps common key names to CDP Input domain parameters.
var keySpecs = map[string]struct {
	code string
	vk   int
}{
	"Enter": {"Enter", 13}, "Backspace": {"Backspace", 8}, "Delete": {"Delete", 46},
	"Tab": {"Tab", 9}, "Escape": {"Escape", 27}, "Space": {"Space", 32},
	"ArrowUp": {"ArrowUp", 38}, "ArrowDown": {"ArrowDown", 40},
	"ArrowLeft": {"ArrowLeft", 37}, "ArrowRight": {"ArrowRight", 39},
	"Home": {"Home", 36}, "End": {"End", 35},
	"PageUp": {"PageUp", 33}, "PageDown": {"PageDown", 34},
}

func pressKey(b *Browser, wsURL, tabID, key string) error {
	if spec, ok := keySpecs[key]; ok {
		params := map[string]any{"code": spec.code, "key": key, "windowsVirtualKeyCode": spec.vk, "nativeVirtualKeyCode": spec.vk}
		if _, err := b.cdpCall(wsURL, tabID, "Input.dispatchKeyEvent", merge(params, map[string]any{"type": "rawKeyDown"})); err != nil {
			return err
		}
		_, err := b.cdpCall(wsURL, tabID, "Input.dispatchKeyEvent", merge(params, map[string]any{"type": "keyUp"}))
		return err
	}
	// Printable key: keydown/char/keyUp so the page sees both the event and
	// the character.
	params := map[string]any{"key": key, "text": key}
	if _, err := b.cdpCall(wsURL, tabID, "Input.dispatchKeyEvent", merge(params, map[string]any{"type": "keyDown"})); err != nil {
		return err
	}
	_, err := b.cdpCall(wsURL, tabID, "Input.dispatchKeyEvent", merge(params, map[string]any{"type": "keyUp"}))
	return err
}

func merge(base, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
