// 订单处理函数（跨文件 handler）。
function listOrders(req, res) {
  const status = req.query.status;
  const page = Number(req.query.page || 1);
  res.json({ page, items: [{ orderId: 'o-1', status: status || 'open', amount: 12.5 }] });
}

module.exports = { listOrders };
