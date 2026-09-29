// Express 样本服务：用于验证 L4 通用 LLM 前端（无任何语言分析支持）。
const express = require('express');
const { listOrders } = require('./orders');

const app = express();
app.use(express.json());

const router = express.Router();
router.get('/users/:id', getUser);
router.post('/users', createUser);
router.get('/orders', listOrders);
app.use('/api', router);

app.get('/health', (req, res) => res.json({ status: 'ok' }));

function getUser(req, res) {
  const id = req.params.id;
  const verbose = req.query.verbose;
  if (!id) {
    return res.status(400).json({ error: 'missing id' });
  }
  res.json({ id: Number(id), name: 'alice', verbose: verbose === '1' });
}

function createUser(req, res) {
  const { name, email } = req.body;
  if (!name) {
    return res.status(422).json({ error: 'name required' });
  }
  res.status(201).json({ id: 1, name, email });
}

module.exports = app;
