/* KunPay 前端脚本 */
(function () {
  'use strict';

  // ---------- 通用 ----------
  window.$ = function (sel, root) { return (root || document).querySelector(sel); };
  window.$$ = function (sel, root) { return Array.from((root || document).querySelectorAll(sel)); };

  window.copyText = function (text, tip) {
    var done = function () {
      if (tip && window.toast) toast(tip || '已复制');
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, function () { fallback(); });
    } else { fallback(); }
    function fallback() {
      var ta = document.createElement('textarea');
      ta.value = text; document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); } catch (e) {}
      document.body.removeChild(ta); done();
    }
  };

  window.toast = function (msg, type) {
    var el = document.createElement('div');
    el.className = 'toast' + (type ? ' ' + type : '');
    el.textContent = msg;
    el.style.cssText = 'position:fixed;top:24px;left:50%;transform:translateX(-50%);background:#1f2329;color:#fff;padding:10px 22px;border-radius:8px;font-size:14px;z-index:9999;box-shadow:0 6px 24px rgba(0,0,0,.18);opacity:0;transition:opacity .2s';
    document.body.appendChild(el);
    requestAnimationFrame(function () { el.style.opacity = '1'; });
    setTimeout(function () { el.style.opacity = '0'; setTimeout(function () { el.remove(); }, 250); }, 2200);
  };

  window.confirmDlg = function (title, body, onOk) {
    var mask = $('#dlgMask');
    if (!mask) {
      mask = document.createElement('div');
      mask.id = 'dlgMask';
      mask.className = 'dlg-mask';
      mask.innerHTML = '<div class="dlg"><h3 id="dlgTitle"></h3><div id="dlgBody" class="muted" style="line-height:1.8"></div>' +
        '<div class="dlg-foot"><button class="btn btn-ghost" id="dlgCancel">取消</button><button class="btn btn-primary" id="dlgOk">确定</button></div></div>';
      document.body.appendChild(mask);
    }
    $('#dlgTitle').textContent = title || '确认操作';
    $('#dlgBody').textContent = body || '';
    mask.classList.add('show');
    var ok = $('#dlgOk'), cancel = $('#dlgCancel');
    var close = function () { mask.classList.remove('show'); ok.onclick = null; cancel.onclick = null; };
    cancel.onclick = close;
    ok.onclick = function () { close(); if (onOk) onOk(); };
  };

  // flash 提示自动消失
  setTimeout(function () {
    $$('.alert.ok, .alert.info').forEach(function (el) {
      setTimeout(function () { el.style.transition = 'opacity .5s'; el.style.opacity = '0'; }, 4000);
    });
  }, 100);

  // 金额格式化: 分 -> 元
  window.yuan = function (cents) {
    var n = Number(cents) || 0;
    var neg = n < 0; n = Math.abs(n);
    var s = Math.floor(n / 100) + '.' + String(n % 100).padStart(2, '0');
    return (neg ? '-' : '') + s;
  };

  // ---------- 收银台 ----------
  var cdEl = $('#countdown');
  if (cdEl) {
    var left = parseInt(cdEl.getAttribute('data-left') || '0', 10);
    var tradeNo = cdEl.getAttribute('data-trade');
    var paid = document.body.getAttribute('data-paid') === '1';
    var pollMs = Math.max(1, parseInt(cdEl.getAttribute('data-poll') || '3', 10) || 3) * 1000;
    var timer = null, poller = null;

    var stopAll = function () { if (timer) clearInterval(timer); if (poller) clearInterval(poller); };
    var renderCd = function () {
      if (left <= 0) {
        stopAll();
        cdEl.innerHTML = '订单已超时,请重新下单';
        return;
      }
      var m = Math.floor(left / 60), s = left % 60;
      cdEl.innerHTML = '支付剩余时间 <b>' + m + '分 ' + (s < 10 ? '0' : '') + s + '秒</b>';
    };
    renderCd();
    timer = setInterval(function () { left--; renderCd(); }, 1000);

    if (!paid) {
      poller = setInterval(function () {
        fetch('/pay/' + tradeNo + '/check', { cache: 'no-store' })
          .then(function (r) { return r.json(); })
          .then(function (j) {
            if (j && j.code === 1 && (j.status === 1 || j.status === 3)) {
              stopAll();
              showPaid(j.return_url || '');
            }
          })
          .catch(function () {});
      }, pollMs);
    }

    var showPaid = function (returnUrl) {
      var card = $('.cashier-card');
      if (!card) return;
      // returnUrl 来自商户,必须用 DOM API 构建(禁止 innerHTML 拼接),且仅接受 http(s)
      if (returnUrl && !/^https?:\/\//i.test(returnUrl)) returnUrl = '';
      var box = document.createElement('div');
      box.className = 'cashier-ok';
      box.innerHTML = '<div class="big"><svg width="38" height="38" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg></div>';
      var h2 = document.createElement('h2');
      h2.style.cssText = 'font-size:22px;margin-bottom:8px';
      h2.textContent = '支付成功';
      var p = document.createElement('p');
      p.className = 'muted';
      p.textContent = returnUrl ? '订单已完成支付,即将跳转...' : '订单已完成支付';
      box.appendChild(h2);
      box.appendChild(p);
      if (returnUrl) {
        var row = document.createElement('div');
        row.className = 'mt';
        var a = document.createElement('a');
        a.className = 'btn btn-primary';
        a.href = returnUrl;
        a.textContent = '立即跳转';
        row.appendChild(a);
        box.appendChild(row);
      }
      card.innerHTML = '';
      card.appendChild(box);
      if (returnUrl) {
        var n = 3;
        var t = setInterval(function () {
          n--;
          if (n > 0) p.textContent = '订单已完成支付,' + n + ' 秒后跳转...';
          if (n <= 0) { clearInterval(t); location.href = returnUrl; }
        }, 1000);
      }
    };
  }

  // ---------- 演示页 ----------
  var demoForm = $('#demoPayBtn');
  if (demoForm) {
    // 初始选中状态与提交类型同步(默认第一个渠道)
    var initType = $('.pay-type.on') || $$('.pay-type')[0];
    if (initType) {
      $$('.pay-type').forEach(function (x) { x.classList.remove('on'); });
      initType.classList.add('on');
      document.body.setAttribute('data-type', initType.getAttribute('data-t'));
    }
    demoForm.addEventListener('click', function () {
      var money = $('#demoMoney').value.trim();
      var type = document.body.getAttribute('data-type') || 'alipay';
      if (!money || parseFloat(money) <= 0) { toast('请输入正确的金额'); return; }
      demoForm.disabled = true; demoForm.textContent = '正在创建订单...';
      var fd = new FormData();
      fd.append('money', money); fd.append('type', type);
      fetch('/demo/create', { method: 'POST', body: fd })
        .then(function (r) { return r.json(); })
        .then(function (j) {
          if (j.code === 1) { location.href = j.pay_url; }
          else { toast(j.msg || '下单失败'); demoForm.disabled = false; demoForm.textContent = '立即支付'; }
        })
        .catch(function () { toast('网络错误'); demoForm.disabled = false; demoForm.textContent = '立即支付'; });
    });
    $$('.pay-type').forEach(function (el) {
      el.addEventListener('click', function () {
        $$('.pay-type').forEach(function (x) { x.classList.remove('on'); });
        el.classList.add('on');
        document.body.setAttribute('data-type', el.getAttribute('data-t'));
      });
    });
  }

  // ---------- 深浅色主题切换 ----------
  var themeBtn = document.getElementById('themeBtn');
  if (themeBtn) {
    themeBtn.addEventListener('click', function () {
      var dark = document.documentElement.getAttribute('data-theme') === 'dark';
      if (dark) {
        document.documentElement.removeAttribute('data-theme');
        localStorage.setItem('kp-theme', 'light');
      } else {
        document.documentElement.setAttribute('data-theme', 'dark');
        localStorage.setItem('kp-theme', 'dark');
      }
      if (window.kpRedrawCharts) window.kpRedrawCharts();
      else location.reload();
    });
  }

  // ---------- 简易柱状图 ----------
  $$('.kbar-chart').forEach(function (canvas) {
    var data = JSON.parse(canvas.getAttribute('data-values') || '[]');
    var labels = JSON.parse(canvas.getAttribute('data-labels') || '[]');
    var ctx = canvas.getContext('2d');
    var W = canvas.width = canvas.offsetWidth * (window.devicePixelRatio || 1);
    var H = canvas.height = canvas.offsetHeight * (window.devicePixelRatio || 1);
    ctx.scale(window.devicePixelRatio || 1, window.devicePixelRatio || 1);
    var w = canvas.offsetWidth, h = canvas.offsetHeight;
    var max = Math.max.apply(null, data.concat([1]));
    var pad = 28, gap = 14;
    var bw = (w - pad * 2 - gap * (data.length - 1)) / data.length;
    // 网格线(跟随主题)
    var cs = getComputedStyle(document.documentElement);
    var gridColor = (cs.getPropertyValue('--chart-grid') || '#f0f1f4').trim();
    ctx.strokeStyle = gridColor;
    ctx.lineWidth = 1;
    for (var g = 0; g <= 4; g++) {
      var gy = 10 + (h - 50) * g / 4;
      ctx.beginPath(); ctx.moveTo(pad, gy); ctx.lineTo(w - pad, gy); ctx.stroke();
    }
    data.forEach(function (v, i) {
      var bh = Math.max(2, (h - 62) * v / max);
      var x = pad + i * (bw + gap);
      var y = h - 34 - bh;
      var grad = ctx.createLinearGradient(0, y, 0, h - 34);
      grad.addColorStop(0, '#5b8bff'); grad.addColorStop(1, '#2e63f6');
      ctx.fillStyle = grad;
      roundRect(ctx, x, y, bw, bh, 5);
      ctx.fill();
      ctx.fillStyle = (getComputedStyle(document.documentElement).getPropertyValue('--text-3') || '#86909c').trim();
      ctx.font = '11px sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText(labels[i] || i, x + bw / 2, h - 14);
      if (v > 0) {
        ctx.fillStyle = (getComputedStyle(document.documentElement).getPropertyValue('--text-2') || '#4e5969').trim();
        ctx.fillText(String(v), x + bw / 2, y - 6);
      }
    });
    function roundRect(c, x, y, w2, h2, r) {
      c.beginPath();
      c.moveTo(x + r, y);
      c.arcTo(x + w2, y, x + w2, y + h2, r);
      c.arcTo(x + w2, y + h2, x, y + h2, r);
      c.arcTo(x, y + h2, x, y, r);
      c.arcTo(x, y, x + w2, y, r);
      c.closePath();
    }
  });

  // 危险操作确认(带 data-confirm 的表单)
  $$('form[data-confirm]').forEach(function (f) {
    f.addEventListener('submit', function (e) {
      e.preventDefault();
      confirmDlg(f.getAttribute('data-confirm-title') || '确认操作', f.getAttribute('data-confirm'), function () {
        document.body.classList.add('kp-loading', 'kp-leaving');
        f.submit();
      });
    });
  });

  // ---------- 防重复提交 ----------
  // 表单提交后禁用全部 submit 按钮,防止网络慢时双击造成重复下单/重复提现。
  // data-confirm 表单会在确认后以 f.submit() 重发(不触发 submit 事件),不受影响;
  // 处理器内已 preventDefault 的表单(如 fetch 型)会跳过;8 秒兜底恢复。
  document.addEventListener('submit', function (e) {
    if (e.defaultPrevented) return;
    var f = e.target;
    if (!f || f.getAttribute('data-guard') === 'off') return;
    var btns = f.querySelectorAll('button[type="submit"]');
    for (var i = 0; i < btns.length; i++) btns[i].disabled = true;
    setTimeout(function () {
      for (var i = 0; i < btns.length; i++) btns[i].disabled = false;
    }, 8000);
  }, false);

  // ---------- 翻页加载特效 ----------
  var reduceMotion = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (!reduceMotion) {
    // 顶部进度条
    var bar = document.createElement('div');
    bar.id = 'kpBar';
    bar.innerHTML = '<i></i>';
    document.body.appendChild(bar);

    var startLoading = function () {
      try { sessionStorage.setItem('kpNavAt', String(Date.now())); } catch (e) {}
      document.body.classList.add('kp-loading');
      document.body.classList.add('kp-leaving');
    };
    var stopLoading = function () {
      document.body.classList.remove('kp-loading');
      document.body.classList.remove('kp-leaving');
    };
    // 新页面打开时,若刚由站内跳转而来,播放一次"到达完成"进度条
    try {
      var navAt = parseInt(sessionStorage.getItem('kpNavAt') || '0', 10);
      if (navAt && Date.now() - navAt < 1200) {
        sessionStorage.removeItem('kpNavAt');
        document.body.classList.add('kp-loading');
        setTimeout(stopLoading, 480);
      }
    } catch (e) {}
    window.addEventListener('pageshow', stopLoading);

    var isInternal = function (el) {
      if (!el) return false;
      var href = el.getAttribute('href') || '';
      if (!href || href.charAt(0) === '#' || href.indexOf('javascript:') === 0 || href.indexOf('mailto:') === 0) return false;
      if (el.target === '_blank' || el.hasAttribute('download')) return false;
      try { return new URL(href, location.href).origin === location.origin; } catch (e) { return false; }
    };
    // 拦截站内链接与表单提交 → 显示进度条
    document.addEventListener('click', function (e) {
      var a = e.target.closest('a');
      if (isInternal(a)) startLoading();
    }, true);
    document.addEventListener('submit', function () { startLoading(); }, true);
    // 防止异常情况下进度条残留
    setTimeout(stopLoading, 8000);
  }

  // ---------- 顶栏用户下拉菜单 ----------
  var userBtn = $('#userBtn');
  if (userBtn) {
    var userMenu = $('#userMenu');
    userBtn.addEventListener('click', function (e) {
      e.stopPropagation();
      userMenu.classList.toggle('open');
    });
    document.addEventListener('click', function (e) {
      if (userMenu.classList.contains('open') && !e.target.closest('.user-menu')) {
        userMenu.classList.remove('open');
      }
    });
  }

  // ---------- 移动端菜单 ----------
  // 文档页宽表格包滚动容器
  $$('.doc-body table').forEach(function (t) {
    var w = document.createElement('div');
    w.className = 'table-wrap';
    t.parentNode.insertBefore(w, t);
    w.appendChild(t);
  });
  var navBtn = $('#kpNavBtn');
  if (navBtn) {
    navBtn.addEventListener('click', function (e) {
      e.stopPropagation();
      document.body.classList.toggle('mnav-open');
    });
    document.addEventListener('click', function (e) {
      if (document.body.classList.contains('mnav-open') && !e.target.closest('.m-panel') && !e.target.closest('#kpNavBtn')) {
        document.body.classList.remove('mnav-open');
      }
    });
  }
  var dashBtn = $('#kpDashBtn');
  if (dashBtn) {
    dashBtn.addEventListener('click', function () {
      document.body.classList.toggle('side-open');
    });
    var mask = $('#sideMask');
    if (mask) mask.addEventListener('click', function () { document.body.classList.remove('side-open'); });
  }
})();
