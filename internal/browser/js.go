// Injected-page JavaScript for the CDP-backed browser actions. Each builder
// returns a self-contained expression string evaluated via Runtime.evaluate
// with returnByValue. The snapshot/aria walkers intentionally mirror the
// desktop IAB's collectors (interactive-element selector list, e1..eN refs,
// css path selectors) so the engine's ref-based flows behave identically.

package browser

import (
	"fmt"
	"strings"
)

// cssPathForJS defines a jsPath(el) helper producing a deterministic CSS
// selector: prefer #id, else walk :nth-of-type chains up to an element with
// an id (or body).
const cssPathHelperJS = `
function __zqfCssPath(el){
  if(!(el instanceof Element)) return "";
  if(el.id) return "#"+CSS.escape(el.id);
  const parts=[];
  let node=el;
  while(node && node.nodeType===1 && node!==document.body){
    let sel=node.tagName.toLowerCase();
    if(node.id){parts.unshift("#"+CSS.escape(node.id));return parts.join(">");}
    const parent=node.parentElement;
    if(parent){
      let i=1,n=node;
      while((n=n.previousElementSibling))i++;
      sel+=":nth-child("+i+")";
    }
    parts.unshift(sel);
    node=parent;
  }
  parts.unshift("body");
  return parts.join(">")+(parts.length===1?"":"");
}
function __zqfUniquePath(el){
  const base=__zqfCssPath(el);
  try{
    if(document.querySelector(base)===el) return base;
    // nth-child can still be ambiguous after DOM edits; disambiguate with a
    // temporary marker attribute.
    const uid="r"+Math.random().toString(36).slice(2,10);
    el.setAttribute("data-zqf-tmp",uid);
    const sel="[data-zqf-tmp="+uid+"]";
    if(document.querySelector(sel)===el) return sel;
    el.removeAttribute("data-zqf-tmp");
  }catch(e){}
  return base;
}`

// roleJS maps elements to ARIA-ish roles (subset of the desktop's
// implicitRole logic — enough for readable snapshots).
const roleHelperJS = `
function __zqfRole(el){
  const r=el.getAttribute("role");
  if(r) return r;
  const t=el.tagName.toLowerCase();
  if(t==="a"&&el.hasAttribute("href"))return"link";
  if(t==="button")return"button";
  if(t==="select")return"combobox";
  if(t==="textarea")return"textbox";
  if(t==="img")return"img";
  if(t==="input"){
    const ty=(el.getAttribute("type")||"text").toLowerCase();
    if(ty==="checkbox")return"checkbox";
    if(ty==="radio")return"radio";
    if(ty==="file")return"button";
    if(["button","submit","reset"].includes(ty))return"button";
    if(ty==="search")return"searchbox";
    return"textbox";
  }
  if(/^h[1-6]$/.test(t))return"heading";
  if(t==="nav")return"navigation";
  if(t==="main")return"main";
  if(t==="header")return"banner";
  if(t==="footer")return"contentinfo";
  if(t==="form")return"form";
  if(t==="ul"||t==="ol")return"list";
  if(t==="li")return"listitem";
  if(t==="table")return"table";
  return"";
}
function __zqfName(el){
  return String(el.getAttribute("aria-label")||el.getAttribute("alt")||el.getAttribute("title")||el.getAttribute("placeholder")||el.getAttribute("aria-placeholder")||"").trim();
}
function __zqfText(el,n){
  return String((el.innerText||el.textContent||"")).trim().replace(/\\s+/g," ").slice(0,n||120);
}
function __zqfVisible(el){
  const s=window.getComputedStyle(el);
  const r=el.getBoundingClientRect();
  if(s.display==="none"||s.visibility==="hidden"||s.opacity==="0")return false;
  if(r.width<=0&&r.height<=0)return false;
  return true;
}`

// snapshotJS collects interactive elements with refs plus a DOM outline.
func snapshotJS() string {
	return `(function(){
` + cssPathHelperJS + roleHelperJS + `
  const INTERACTIVE="a[href],button,input,textarea,select,[role],[onclick],[tabindex],summary,label,[contenteditable]";
  const all=[...document.querySelectorAll(INTERACTIVE)].filter(e=>__zqfVisible(e));
  const truncated=all.length>120;
  const els=all.slice(0,120);
  const refSelectors={};
  const elements=els.map((e,i)=>{
    const ref="e"+(i+1);
    const sel=__zqfUniquePath(e);
    refSelectors[ref]=sel;
    const r=e.getBoundingClientRect();
    const o={ref,tag:e.tagName.toLowerCase(),cssPath:sel};
    const role=__zqfRole(e);
    if(role)o.role=role;
    const name=__zqfName(e);
    const text=__zqfText(e,100);
    if(name)o.name=name;
    if(text&&text!==name)o.text=text;
    if("value" in e && String(e.value||"")!=="")o.value=String(e.value).slice(0,240);
    if("placeholder" in e && e.placeholder)o.placeholder=e.placeholder;
    if("disabled" in e && e.disabled)o.disabled=true;
    if("checked" in e)o.checked=!!e.checked;
    o.rect={x:Math.round(r.x),y:Math.round(r.y),width:Math.round(r.width),height:Math.round(r.height)};
    o.inViewport=r.top>=0&&r.bottom<=(window.innerHeight||document.documentElement.clientHeight);
    return o;
  });
  const STRUCTURAL="body,main,nav,header,footer,aside,section,article,h1,h2,h3,h4,h5,h6,p,ul,ol,table,form,fieldset,figure";
  const dom=[...document.querySelectorAll(STRUCTURAL)].filter(e=>__zqfVisible(e)).slice(0,300).map(e=>{
    let depth=0;
    for(let p=e.parentElement;p&&p!==document.body;p=p.parentElement)depth++;
    const o={tag:e.tagName.toLowerCase(),depth};
    const role=__zqfRole(e);
    if(role)o.role=role;
    const name=__zqfName(e);
    const text=/^(h[1-6]|p|li|dt|dd|blockquote|pre|code|caption|th|td|label|summary|button|a|option|legend|figcaption)$/.test(e.tagName.toLowerCase())?__zqfText(e,300):"";
    if(name)o.name=name;
    if(text)o.text=text;
    return o;
  });
  return {url:location.href,title:document.title,elements,truncated,dom,domTruncated:false,refSelectors};
})()`
}

// ariaSnapshotJS renders a Playwright-ariaSnapshot-like YAML tree.
func ariaSnapshotJS() string {
	return `(function(){
` + cssPathHelperJS + roleHelperJS + `
  const nodes=[...document.body.querySelectorAll("a[href],button,input,textarea,select,h1,h2,h3,h4,h5,h6,[role],img,li,td,th,option,label,summary,[contenteditable],[onclick],[tabindex]")].filter(e=>__zqfVisible(e));
  const refs={};
  let counter=0, out=[];
  for(const e of nodes){
    const role=__zqfRole(e);
    if(!role)continue;
    const depth=(()=>{let d=0;for(let p=e.parentElement;p&&p!==document.body;p=p.parentElement)d++;return d;})();
    if(depth>14)continue;
    let ref="";
    if(/^(link|button|textbox|searchbox|combobox|checkbox|radio|slider|menuitem|tab|option|switch)$/.test(role)){
      ref="e"+(++counter);
      refs[ref]=__zqfUniquePath(e);
    }
    const name=__zqfName(e)||__zqfText(e,80);
    const parts=[];
    let line="- ".repeat(depth+1)+role;
    if(name)line+=' "'+name.replace(/"/g,"'")+'"';
    if(ref)line+=" [ref="+ref+"]";
    if(/^(h[1-6])$/.test(e.tagName.toLowerCase()))line+=" [level="+e.tagName[1]+"]";
    if("checked" in e && e.checked)line+=" [checked]";
    if(e instanceof HTMLInputElement && e.value && /^(textbox|searchbox)$/.test(role))line+=' [value="'+String(e.value).slice(0,60).replace(/"/g,"'")+'"]';
    out.push(line);
  }
  return {yaml: out.join("\\n"), refs};
})()`
}

// elementInfoJS mirrors the desktop elementInfo evaluate (elementsFromPoint).
func elementInfoJS(x, y float64) string {
	return fmt.Sprintf(`(function(){
` + cssPathHelperJS + `
  return document.elementsFromPoint(%f,%f)
    .filter(e=>e.matches("a,button,input,select,textarea,[role],[tabindex]"))
    .map(e=>{
      const r=e.getBoundingClientRect();
      return {tagName:e.tagName.toLowerCase(),role:e.getAttribute("role"),
        visibleText:(e.innerText||e.textContent||"").trim()||null,
        ariaName:e.getAttribute("aria-label")||(e.innerText||e.textContent||"").trim()||null,
        boundingBox:{x:r.x,y:r.y,width:r.width,height:r.height},
        preview:e.outerHTML.slice(0,300),
        selector:{primary:e.id?(""+document.querySelector("#"+CSS.escape(e.id))===e?"#"+e.id:e.tagName.toLowerCase()):e.tagName.toLowerCase(),candidates:[]},
        cssPath:__zqfUniquePath(e)};
    });
})()`, x, y)
}

// elementAtRectJS returns the bounding rect of the first interactive element
// at a point (for elementScreenshot).
func elementAtRectJS(x, y float64) string {
	return fmt.Sprintf(`(function(){
  const el=document.elementsFromPoint(%f,%f).find(e=>e.matches("a,button,input,select,textarea,[role],[tabindex]"));
  if(!el)return null;
  const r=el.getBoundingClientRect();
  return {x:r.x,y:r.y,width:r.width,height:r.height};
})()`, x, y)
}

// elementRectJS resolves a css selector to its bounding rect.
func elementRectJS(css string) string {
	return fmt.Sprintf(`(function(){const e=document.querySelector(%s);if(!e)return null;e.scrollIntoView({block:"center"});const r=e.getBoundingClientRect();return {x:r.x,y:r.y,width:r.width,height:r.height};})()`, jsQuote(css))
}

// fillJS sets an input's value with native setter + input/change events so
// framework listeners fire.
func fillJS(css, value string) string {
	return fmt.Sprintf(`(function(){
  const e=document.querySelector(%s);
  if(!e)throw new Error("not found: element");
  e.scrollIntoView({block:"center"});
  e.focus();
  const proto=e instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;
  const setter=Object.getOwnPropertyDescriptor(proto,"value").set;
  setter.call(e,%s);
  e.dispatchEvent(new Event("input",{bubbles:true}));
  e.dispatchEvent(new Event("change",{bubbles:true}));
})()`, jsQuote(css), jsQuote(value))
}

// selectOptionJS selects options by value / "label:x" / "index:n" and fires
// change.
func selectOptionJS(css string, values []string) string {
	vals := make([]string, 0, len(values))
	for _, v := range values {
		vals = append(vals, jsQuote(v))
	}
	return fmt.Sprintf(`(function(){
  const e=document.querySelector(%s);
  if(!e)throw new Error("not found: select");
  const specs=[%s];
  for(const s of specs){
    if(s.startsWith("label:")){
      const opt=[...e.options].find(o=>o.label===s.slice(6));
      if(opt)opt.selected=true;
    }else if(s.startsWith("index:")){
      const i=parseInt(s.slice(6),10);
      if(e.options[i])e.options[i].selected=true;
    }else{
      const opt=[...e.options].find(o=>o.value===s);
      if(opt)opt.selected=true;
    }
  }
  e.dispatchEvent(new Event("input",{bubbles:true}));
  e.dispatchEvent(new Event("change",{bubbles:true}));
})()`, jsQuote(css), strings.Join(vals, ","))
}

// waitForProbeJS returns true when the waitFor condition holds.
func waitForProbeJS(selector, text, textGone string) string {
	conds := "true"
	if selector != "" {
		conds = fmt.Sprintf("!!document.querySelector(%s)", jsQuote(selector))
	}
	if text != "" {
		conds += fmt.Sprintf(" && document.body && document.body.innerText.includes(%s)", jsQuote(text))
	}
	if textGone != "" {
		conds += fmt.Sprintf(" && !(document.body && document.body.innerText.includes(%s))", jsQuote(textGone))
	}
	return "(function(){try{return " + conds + ";}catch(e){return false;}})()"
}

func isVisibleJS(css string) string {
	return fmt.Sprintf(`(function(){const e=document.querySelector(%s);if(!e)return false;
const s=window.getComputedStyle(e);const r=e.getBoundingClientRect();
return s.display!=="none"&&s.visibility!=="hidden"&&s.opacity!=="0"&&(r.width>0||r.height>0);})()`, jsQuote(css))
}

func locatorStateJS(css, state string) string {
	switch state {
	case "attached":
		return fmt.Sprintf("(function(){return !!document.querySelector(%s);})()", jsQuote(css))
	case "detached":
		return fmt.Sprintf("(function(){return !document.querySelector(%s);})()", jsQuote(css))
	case "hidden":
		return fmt.Sprintf(`(function(){const e=document.querySelector(%s);if(!e)return true;
const s=window.getComputedStyle(e);const r=e.getBoundingClientRect();
return s.display==="none"||s.visibility==="hidden"||s.opacity==="0"||(r.width<=0&&r.height<=0);})()`, jsQuote(css))
	case "visible":
		return isVisibleJS(css)
	}
	return "true"
}

// locatorEvaluateJS runs an expression with `el` bound to the matched element
// (function form) or evaluated as a plain expression against it.
func locatorEvaluateJS(css, expr string) string {
	return fmt.Sprintf(`(function(){
  const el=document.querySelector(%s);
  if(!el)throw new Error("not found: element");
  const fn=%s;
  if(typeof fn==="function")return fn(el);
  return el;
})()`, jsQuote(css), jsQuote(expr))
}
