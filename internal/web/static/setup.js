(function () {
  function setupRows(kind) {
    var rows = document.querySelector("[data-" + kind + "-rows]");
    var count = document.querySelector("[data-" + kind + "-count]");
    var add = document.querySelector("[data-add-setup-" + kind + "]");
    var template = document.getElementById("setup-" + kind + "-template");
    var limit = document.querySelector("[data-" + kind + "-limit]");
    if (!rows || !count || !add || !template) return;
    add.addEventListener("click", function () {
      var index = Number(count.value);
      if (index >= 20) {
        limit.hidden = false;
        add.disabled = true;
        return;
      }
      var item = template.content.cloneNode(true);
      item.querySelectorAll("[id], [for], [name]").forEach(function (el) {
        ["id", "for", "name"].forEach(function (attribute) {
          var value = el.getAttribute(attribute);
          if (value) el.setAttribute(attribute, value.replace(/__INDEX__/g, index).replace(/__INDEX_LABEL__/g, index + 1));
        });
      });
      rows.appendChild(item);
      count.value = index + 1;
      if (index + 1 >= 20) {
        limit.hidden = false;
        add.disabled = true;
      }
    });
  }
  setupRows("expense");
  setupRows("income");
})();
