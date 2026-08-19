/* drivelite — grid/list toggle, sorting, and a keyboard-driven media lightbox.
   No inline script anywhere, so a strict Content-Security-Policy still applies. */
(function () {
  "use strict";

  var app = document.getElementById("app");
  if (!app) return;

  /* ---------- preferences ---------- */

  function setCookie(name, value) {
    document.cookie =
      name + "=" + encodeURIComponent(value) + ";path=/;max-age=31536000;samesite=lax";
  }

  document.querySelectorAll("[data-autosubmit]").forEach(function (el) {
    el.addEventListener("change", function () {
      el.form.submit();
    });
  });

  document.querySelectorAll("[data-view]").forEach(function (btn) {
    if (btn === app) return;
    btn.addEventListener("click", function () {
      var mode = btn.getAttribute("data-view");
      var list = app.querySelector(".items");
      if (list) list.className = "items " + mode;
      app.setAttribute("data-view", mode);
      document.querySelectorAll("button[data-view]").forEach(function (b) {
        b.setAttribute("aria-pressed", String(b.getAttribute("data-view") === mode));
      });
      setCookie("dl_view", mode);
    });
  });

  /* ---------- media list, read straight from the rendered DOM ---------- */

  var media = [];
  Array.prototype.forEach.call(app.querySelectorAll(".item[data-media]"), function (li) {
    var kind = li.getAttribute("data-kind");
    if (kind !== "image" && kind !== "video" && kind !== "audio") return;
    media.push({
      kind: kind,
      src: li.getAttribute("data-media"),
      dl: li.getAttribute("data-dl"),
      name: li.getAttribute("data-name"),
      playable: li.getAttribute("data-playable") === "1",
      node: li,
    });
    li.setAttribute("data-mindex", String(media.length - 1));
  });

  var box = document.getElementById("lightbox");
  var stage = document.getElementById("lb-stage");
  var title = document.getElementById("lb-title");
  var count = document.getElementById("lb-count");
  var dlLink = document.getElementById("lb-dl");
  var prevBtn = document.getElementById("lb-prev");
  var nextBtn = document.getElementById("lb-next");
  var current = -1;
  var lastFocus = null;

  function clearStage() {
    // Pause any playing media before discarding the element.
    var v = stage.querySelector("video, audio");
    if (v) {
      v.pause();
      v.removeAttribute("src");
      v.load();
    }
    stage.textContent = "";
  }

  function icon(id) {
    var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "i");
    var use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", "#" + id);
    svg.appendChild(use);
    return svg;
  }

  function show(index) {
    if (index < 0 || index >= media.length) return;
    current = index;
    var item = media[index];

    clearStage();
    title.textContent = item.name;
    count.textContent = index + 1 + " of " + media.length;
    dlLink.setAttribute("href", item.dl);

    if (item.kind === "image") {
      var img = document.createElement("img");
      img.src = item.src;
      img.alt = item.name;
      stage.appendChild(img);
    } else if (item.playable) {
      var el = document.createElement(item.kind === "audio" ? "audio" : "video");
      el.src = item.src;
      el.controls = true;
      el.autoplay = true;
      el.preload = "metadata";
      stage.appendChild(el);
    } else {
      // Formats no browser plays natively — AVCHD/.MTS being the usual case.
      var box2 = document.createElement("div");
      box2.className = "lb-fallback";
      box2.appendChild(icon("i-video"));

      var p = document.createElement("p");
      p.textContent = "This format can't be played in the browser.";
      box2.appendChild(p);

      var a = document.createElement("a");
      a.className = "btn primary";
      a.href = item.dl;
      a.appendChild(icon("i-download"));
      a.appendChild(document.createTextNode(" Download " + item.name));
      box2.appendChild(a);

      stage.appendChild(box2);
    }

    prevBtn.disabled = index === 0;
    nextBtn.disabled = index === media.length - 1;
  }

  function open(index) {
    lastFocus = document.activeElement;
    box.hidden = false;
    document.body.style.overflow = "hidden";
    show(index);
    nextBtn.focus();
  }

  function close() {
    clearStage();
    box.hidden = true;
    document.body.style.overflow = "";
    if (lastFocus && lastFocus.focus) lastFocus.focus();
    current = -1;
  }

  // Intercept clicks on previewable items; everything else follows its href.
  app.addEventListener("click", function (ev) {
    var link = ev.target.closest ? ev.target.closest("a.hit") : null;
    if (!link) return;
    if (ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.button !== 0) return;

    var li = link.closest(".item");
    if (!li || !li.hasAttribute("data-mindex")) return;

    ev.preventDefault();
    open(parseInt(li.getAttribute("data-mindex"), 10));
  });

  prevBtn.addEventListener("click", function () { show(current - 1); });
  nextBtn.addEventListener("click", function () { show(current + 1); });
  document.getElementById("lb-close").addEventListener("click", close);

  box.addEventListener("click", function (ev) {
    // Clicking the backdrop (not the media itself) dismisses the viewer.
    if (ev.target === box || ev.target === stage) close();
  });

  document.addEventListener("keydown", function (ev) {
    if (box.hidden) {
      // "/" focuses the search field, as long as we are not already typing.
      if (ev.key === "/" && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) {
        var q = document.querySelector(".search input");
        if (q) { ev.preventDefault(); q.focus(); }
      }
      return;
    }
    if (ev.key === "Escape") { ev.preventDefault(); close(); }
    else if (ev.key === "ArrowLeft") { ev.preventDefault(); show(current - 1); }
    else if (ev.key === "ArrowRight") { ev.preventDefault(); show(current + 1); }
  });

  // Thumbnails that fail to render fall back to the generic type icon.
  Array.prototype.forEach.call(app.querySelectorAll(".thumb img"), function (img) {
    img.addEventListener("error", function () {
      var wrap = img.parentNode;
      var kind = img.closest(".item").getAttribute("data-kind");
      img.remove();
      var svg = icon("i-" + (kind === "video" ? "video" : "image"));
      svg.setAttribute("class", "i tile");
      wrap.insertBefore(svg, wrap.firstChild);
    });
  });
})();
