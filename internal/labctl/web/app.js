"use strict";
(function () {
const TOKEN = new URLSearchParams(location.search).get("t") || "";
const $ = id => document.getElementById(id);
const esc = s => String(s==null?"":s).replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
let state = null, es = null, lab = null, kinds = [];

async function api(path, body){
  const o = {headers:{"X-Lab-Token":TOKEN},cache:"no-store"};
  if(body!==undefined){o.method="POST";o.headers["Content-Type"]="application/json";o.body=JSON.stringify(body);}
  const r = await fetch(path+(path.includes("?")?"&":"?")+"t="+TOKEN, o);
  return r.json().catch(()=>({ok:false,error:"HTTP "+r.status}));
}

/* ---- pivot / views ---- */
function go(view){
  document.querySelectorAll(".view").forEach(v=>v.classList.toggle("hidden", v.id!==view));
  document.querySelectorAll(".ptab").forEach(t=>t.setAttribute("aria-selected", t.dataset.go===view ? "true":"false"));
  if(view==="config") fillCfg();
}
document.querySelectorAll(".ptab").forEach(t=>t.onclick=()=>go(t.dataset.go));

const HOST_IDS = ["host","hyperv","rights"];
function checkOK(id){ const c=(state.checks||[]).find(x=>x.id===id); return !c || c.ok; }
function hostReady(){ return HOST_IDS.every(checkOK); }

async function refresh(){
  const s = await api("/api/state");
  if(!s.ok) return;
  state = s; lab = s.lab; kinds = s.kinds || [];
  $("labname").textContent = lab.name ? "· "+lab.name : "";
  $("labpath").textContent = s.lab_path + (s.saved ? "" : " (not saved yet)");
  $("hostbanner").classList.toggle("hidden", s.host_ok);
  $("hostbadge").textContent = s.host_ok ? (s.ready ? "Hyper-V host · ready" : "Hyper-V host · setup") : "UI only";
  if(!s.host_ok) $("hostmsg").textContent = s.host_msg+" Config and the UI work here; VM actions need the Windows host.";
  $("notes").innerHTML = (s.warnings||[]).map(w=>`<div class="infobar warn" style="margin-top:8px"><span class="ii">!</span><span>${esc(w)}</span></div>`).join("");
  $("credsok").classList.toggle("hidden", !s.creds_set);
  renderChecks();
  renderRows();
  $("notready").classList.toggle("hidden", s.ready);
  const setupTab = document.querySelector('.ptab[data-go="setup"]');
  setupTab.classList.toggle("done", s.ready);
  const busy = s.busy;
  $("savecreds").disabled = busy; $("savecfg").disabled = busy;
  $("resetall").disabled = busy || !hostReady() || !(state.machines||[]).some(m=>m.has_baseline);
  $("labinfo").href = "/api/lab-info?t="+TOKEN;
  $("jobline").textContent = s.job ? (s.job.title+" · "+s.job.status) : (busy?"busy":"idle");
  if(s.job && (!es || es._id!==s.job.id) && s.job.status==="running"){ tail(s.job.id); }
}

function renderChecks(){
  const ul = $("checks"); ul.innerHTML = "";
  (state.checks||[]).forEach(c=>{
    const cls = c.ok ? "" : (c.required ? "c-fail" : "c-warn");
    const ic = c.ok ? "✓" : (c.required ? "✕" : "!");
    const li = document.createElement("li"); li.className = cls;
    li.innerHTML = `<span class="ic">${ic}</span><div><div class="lbl">${esc(c.label)}</div>${c.detail?`<div class="det">${esc(c.detail)}</div>`:""}</div>`;
    ul.appendChild(li);
  });
  const blocking = (state.checks||[]).filter(c=>c.required && !c.ok).length;
  $("readyline").textContent = state.ready ? "Setup complete — continue on the Lab tab." : (blocking+" item(s) to fix before VM actions are enabled.");
}

function kindPill(kind){
  const k = kinds.find(x=>x.kind===kind);
  const os = k ? k.os : "";
  const label = k ? k.label : kind;
  return `<span class="kindpill" title="${esc(label)}"><span class="os ${esc(os)}"></span>${esc(kind)}</span>`;
}

function why(action){
  if(state.busy) return "another action is running";
  if(!hostReady()) return "Setup: the Hyper-V host is not ready";
  if((action==="create"||action==="provision") && !state.ready) return "Setup is not complete";
  return "";
}

function renderRows(){
  const tb = $("rows"); tb.innerHTML = "";
  const ms = state.machines||[];
  $("norows").classList.toggle("hidden", ms.length>0);
  ms.forEach(m=>{
    const dot = m.vm_state==="Running"?"run":(m.vm_state?"off":"none");
    const vm = m.vm_state || (state.host_ok?"absent":"—");
    const base = m.scanner ? "<span class=hint>n/a</span>" : (m.has_baseline ? "✓" : "<span class=hint>—</span>");
    const list = m.scanner ? ["create","provision","start","stop"] : ["create","provision","snapshot","reset","start","stop"];
    const acts = list.map(a=>{
      let w = why(a);
      if(!w && a==="create" && m.vm_state) w = "the VM already exists";
      if(!w && a==="reset" && !m.has_baseline) w = "no baseline yet — provision and snapshot first";
      if(!w && (a==="snapshot"||a==="provision"||a==="start"||a==="stop") && !m.vm_state) w = "create the VM first";
      return `<button data-m="${esc(m.name)}" data-a="${a}"${w?` disabled title="${esc(w)}"`:""}>${a}</button>`;
    }).join("");
    const tr = document.createElement("tr");
    tr.innerHTML = `<td><b>${esc(m.name)}</b></td><td>${kindPill(m.kind)}</td><td>${esc(m.domain||"—")}</td><td class=mono>${esc(m.ip)}</td>`+
      `<td><span class="dot ${dot}"></span>${esc(vm)}</td><td>${base}</td><td><div class="rowact">${acts}</div></td>`;
    tb.appendChild(tr);
  });
  tb.querySelectorAll(".rowact button").forEach(b=>b.onclick=()=>machine(b.dataset.m,b.dataset.a));
}

function line(ev){
  const c = $("console");
  const t = ev;
  let cls = ""; if(t.startsWith("$ "))cls="cmd"; else if(t.startsWith("ERROR"))cls="err"; else if(t.startsWith("» ")||t.startsWith("==="))cls="me";
  const span = document.createElement("span");
  span.className = cls; span.textContent = t+"\n"; c.appendChild(span);
  c.scrollTop = c.scrollHeight;
}
function tail(id){
  if(es){es.close();es=null;}
  $("console").innerHTML = "";
  es = new EventSource("/api/logs?job="+encodeURIComponent(id)+"&t="+TOKEN);
  es._id = id;
  es.addEventListener("log", e=>line(JSON.parse(e.data)));
  es.addEventListener("done", e=>{ line("— "+JSON.parse(e.data)+" —"); es.close(); es=null; setTimeout(refresh,500); });
  es.onerror = ()=>{ if(es){es.close();es=null;} };
}

async function start(call){
  const r = await call();
  if(!r.ok){ line("ERROR: "+r.error); return; }
  if(r.job) tail(r.job);
  setTimeout(refresh, 300);
}
const machine = (name,action)=>start(()=>api("/api/machine",{name,action}));

$("savecreds").onclick = async ()=>{
  await api("/api/creds",{AdminPassword:$("c_admin").value,AuditPassword:$("c_audit").value,AuditUser:$("c_user").value});
  $("c_admin").value=""; $("c_audit").value=""; refresh();
};
$("resetall").onclick = ()=>start(()=>api("/api/reset-all",{}));
$("tosetup").onclick = e=>{ e.preventDefault(); go("setup"); };

/* ---- config editor ---- */
function fillCfg(){
  $("cfgpath").textContent = ""; $("cfgerr").classList.add("hidden");
  $("cfgpath").textContent = state ? state.lab_path : "";
  $("f_switch").value=lab.switch||""; $("f_work").value=lab.work_dir||"";
  $("f_subnet").value=lab.subnet||""; $("f_ssh").value=lab.ssh_user||"";
  const mr = $("mrows"); mr.innerHTML="";
  (lab.machines||[]).forEach((m,i)=>mr.appendChild(mrow(m,i)));
}
function kindOptions(sel){
  return (kinds.length?kinds:[{kind:"windows-dc",label:"windows-dc"}]).map(k=>
    `<option value="${esc(k.kind)}"${k.kind===sel?' selected':''}>${esc(k.label||k.kind)}</option>`).join("");
}
function mrow(m,i){
  const d = document.createElement("div"); d.className="mrow"; d.dataset.i=i;
  const f=(k,v,ph)=>`<label>${k}<input class="inp m_${k}" value="${esc(v==null?'':v)}" placeholder="${ph||''}"></label>`;
  d.innerHTML =
    f("name",m.name)+
    `<label>kind<select class="inp m_kind">${kindOptions(m.kind)}</select></label>`+
    f("domain",m.domain,"da.test")+f("ip",m.ip,"10.55.0.10")+
    `<button class="del">✕</button>`+
    f("iso",m.iso,"path to install ISO")+f("forest_mode",m.forest_mode,"Win2016")+
    f("ssh_user",m.ssh_user,"blank = lab default")+
    f("cpu",m.cpu)+f("mem_gb",m.mem_gb)+f("disk_gb",m.disk_gb);
  d.querySelector(".del").onclick=()=>{ lab.machines.splice(i,1); fillCfg(); };
  return d;
}
$("addm").onclick = ()=>{
  lab.machines=lab.machines||[];
  const n = lab.machines.length;
  const net = (lab.subnet||"10.10.30.0/24").split("/")[0].split(".").slice(0,3).join(".");
  lab.machines.push({name:"DC"+(n+1),kind:"windows-dc",domain:"corp.test",ip:net+"."+(10+n),prefix:24,cpu:2,mem_gb:4,disk_gb:60,forest_mode:"Win2016"});
  fillCfg();
};
$("savecfg").onclick = async ()=>{
  const g=id=>$(id).value.trim();
  const next = {name:lab.name,subnet:g("f_subnet"),ssh_user:g("f_ssh"),switch:g("f_switch"),work_dir:g("f_work"),machines:[]};
  document.querySelectorAll("#mrows .mrow").forEach(d=>{
    const v=k=>{const e=d.querySelector(".m_"+k);return e?e.value.trim():"";};
    const n=k=>parseInt(v(k)||"0",10);
    next.machines.push({name:v("name"),kind:v("kind"),domain:v("domain"),ip:v("ip"),iso:v("iso"),
      forest_mode:v("forest_mode"),ssh_user:v("ssh_user"),prefix:24,cpu:n("cpu"),mem_gb:n("mem_gb"),disk_gb:n("disk_gb")});
  });
  const r = await api("/api/config", next);
  if(!r.ok){ const e=$("cfgerr"); e.textContent=r.error; e.classList.remove("hidden"); return; }
  await refresh(); go(state && state.ready ? "lab" : "setup");
};

/* ---- chrome ---- */
const root=document.documentElement;
$("theme").onclick=()=>{ const n=root.getAttribute("data-theme")==="dark"?"light":"dark"; root.setAttribute("data-theme",n); try{localStorage.setItem("lab-theme",n)}catch(e){} };
$("quit").onclick=async()=>{ await api("/api/quit",{}); document.body.innerHTML='<p style="padding:40px;font:16px system-ui">Lab control panel stopped. You can close this tab.</p>'; };
try{const t=localStorage.getItem("lab-theme"); if(t)root.setAttribute("data-theme",t);}catch(e){}
refresh(); setInterval(()=>{ if(!es) refresh(); }, 5000);
})();
