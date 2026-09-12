/* zqf 回车提交补丁 —— 注入到镜像版 /remote/v4 页面。
 *
 * 官方 bundle 在「web 远控 + 手机视口」（(max-width:767px) and (hover:none)
 * and (pointer:coarse)）下把 composer 的 enterSubmits 硬编码为 false
 * （bundle 内 MLe(): return !(preferEnterNewline && isMobileTextInputViewport)），
 * 于是手机上回车永远只插换行，任务只能靠点发送箭头提交；而消息编辑条
 * （rewind/editUserQuery）的 enterSubmits 却保持 true——同一个页面两种回车
 * 习惯。本补丁在手机视口下恢复「回车提交」：第一下回车交给输入法上屏确认
 * （组词中绝不触发提交），第二下回车提交任务；Shift+Enter 仍为换行。
 *
 * 拦截方式：document 捕获阶段拦在所有 app 监听器之前，命中时
 * preventDefault + stopPropagation（Lexical 因此不再插换行），改为点击
 * composer 的发送按钮（chat-send-button，与手点发送完全同一条提交链路）。
 * 桌面宽度（媒体查询不匹配）一律放行原生逻辑，镜像的验收用途不受影响。
 */
(function () {
  'use strict';

  var MOBILE_MQ = '(max-width: 767px) and (hover: none) and (pointer: coarse)';

  // 组词状态计数：部分输入法（尤其 Firefox / 个别安卓）先发 compositionend
  // 再发确认回车（isComposing 已为 false），只看 isComposing 会漏。
  var composing = 0;
  var composingTimer = 0;

  document.addEventListener('compositionstart', function () {
    composing++;
    if (composingTimer) { clearTimeout(composingTimer); composingTimer = 0; }
  }, true);
  document.addEventListener('compositionend', function () {
    if (composingTimer) clearTimeout(composingTimer);
    composingTimer = setTimeout(function () {
      composing = Math.max(0, composing - 1);
      composingTimer = 0;
    }, 50);
  }, true);

  function composerRegion(node) {
    while (node) {
      if (node.nodeType === 1 && node.classList && node.classList.contains('chat-composer-region')) {
        return node;
      }
      node = node.parentNode || node.host || null;
    }
    return null;
  }

  function findSendButton(region) {
    var b = region.querySelector('button[data-testid="chat-send-button"]');
    if (b && !b.disabled) return b;
    var all = region.querySelectorAll('button[type="submit"]');
    for (var i = all.length - 1; i >= 0; i--) {
      if (!all[i].disabled) return all[i];
    }
    return null;
  }

  function popupOpen() {
    // @ / # / $ 提及菜单、/ 命令菜单（cmdk）以及 radix 弹层打开时，
    // 回车属于「选中候选项」，绝不能抢来提交。
    return !!document.querySelector('[cmdk-root], [role="listbox"], [data-radix-popper-content-wrapper]');
  }

  document.addEventListener('keydown', function (e) {
    try {
      if (e.key !== 'Enter') return;
      if (e.shiftKey || e.ctrlKey || e.metaKey || e.altKey) return;
      if (!window.matchMedia(MOBILE_MQ).matches) return;
      if (e.isComposing || e.keyCode === 229 || composing > 0) return;
      if (popupOpen()) return;
      var t = e.target;
      if (!t || t.nodeType !== 1 || !t.closest) return;
      var editable = t.closest('[contenteditable="true"],[contenteditable=""],textarea');
      if (!editable) return;
      var region = composerRegion(editable);
      if (!region) return;
      var btn = findSendButton(region);
      if (!btn) return; // 无可用发送按钮（如空文案禁用）→ 保持原生行为
      e.preventDefault();
      e.stopPropagation();
      btn.click();
    } catch (err) {
      // 补丁异常绝不影响原生输入
    }
  }, true);
})();
