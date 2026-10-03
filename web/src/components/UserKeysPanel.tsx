/**
 * 用户自备密钥（BYOK）管理面板。
 *
 * 意图（Why）：
 *   让用户托管自己的上游凭据（如 NVIDIA 官方 NIM 的 API Key），
 *   从而用【自己的额度】通过本网关的统一协议、鉴权与审计访问模型。
 *   费用由上游直接向用户收取，站内不重复扣费。
 *
 * 安全设计（本文件最重要的部分）：
 *   1) **绝不回填明文**。编辑弹层里 API Key 输入框永远为空，
 *      留空即"不修改"。用户看到的只有后端返回的掩码（nvap***cdef）。
 *      理由：明文每次回传都会经过浏览器、网络与各类日志——
 *      密钥管理页的响应是最不该出现明文的地方。
 *   2) **不把掩码当占位符回填**。看起来"更方便"，但会让用户以为
 *      掩码本身是密钥内容，误改后连原值都找不回。
 *   3) provider 只能从后端白名单里选，不允许自由输入——
 *      否则用户可以让平台代他向任意地址发带凭据的请求。
 *
 * 状态语义必须区分清楚：
 *   - "已停用"是用户主动关的；
 *   - "暂时熔断"是平台因连续失败自动隔离的。
 *   两者界面上都表现为"不可用"，但用户对二者的处置动作完全相反
 *   （前者去设置里打开，后者等平台恢复），混为一谈会让人反复开关试图"修复"。
 */
'use client'

import { useCallback, useEffect, useMemo, useState } from 'react'

import {
  createUserKey,
  deleteUserKey,
  fetchKeyProviders,
  listUserKeys,
  updateUserKey,
} from '@/api/portal'
import type { UserKey, UserKeyPayload, UserKeyProvider } from '@/api/types'
import { Button } from '@/components/ui/Button'
import { Badge, Card, EmptyState, SkeletonRows } from '@/components/ui/Display'
import { Field, Input, Select } from '@/components/ui/Form'
import { Modal } from '@/components/ui/Modal'

/** 弹层表单的本地状态 */
interface FormState {
  provider: string
  label: string
  apiKey: string
  baseUrl: string
  models: string
}

/** 空表单（新增时用） */
function emptyForm(provider = ''): FormState {
  return { provider, label: '', apiKey: '', baseUrl: '', models: '' }
}

export function UserKeysPanel() {
  const [keys, setKeys] = useState<UserKey[]>([])
  const [providers, setProviders] = useState<UserKeyProvider[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // 弹层状态：editing 为 null 表示新增，否则为正在编辑的记录
  const [editing, setEditing] = useState<UserKey | null>(null)
  const [formOpen, setFormOpen] = useState(false)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')
  // 待删除确认的记录：null 表示无待确认项
  const [removing, setRemoving] = useState<UserKey | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const [keyRes, providerRes] = await Promise.all([listUserKeys(), fetchKeyProviders()])
      setKeys(keyRes.keys ?? [])
      setProviders(providerRes.providers ?? [])
    } catch (e) {
      setError(e instanceof Error ? e.message : '加载自备密钥失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  /** 当前选中的 provider 定义（决定默认地址、能否自定义、建议模型） */
  const selectedProvider = useMemo(
    () => providers.find((p) => p.key === form.provider),
    [providers, form.provider],
  )

  /** 打开新增弹层 */
  function openCreate() {
    const first = providers[0]?.key ?? ''
    setEditing(null)
    setForm(emptyForm(first))
    setFormError('')
    setFormOpen(true)
  }

  /**
   * 打开编辑弹层。
   *
   * 关键：api_key 保持空串——**不把掩码回填**。
   * 用户想改密钥就重新填，不改就留空（后端按"不修改"处理）。
   */
  function openEdit(key: UserKey) {
    setEditing(key)
    setForm({
      provider: key.provider,
      label: key.label,
      apiKey: '',
      baseUrl: key.base_url,
      models: (key.models ?? []).join(', '),
    })
    setFormError('')
    setFormOpen(true)
  }

  async function submit() {
    if (!form.provider) {
      setFormError('请选择要接入的上游')
      return
    }
    // 新增必填密钥；编辑时留空表示不修改。
    if (!editing && !form.apiKey.trim()) {
      setFormError('请填写 API Key')
      return
    }
    setSaving(true)
    setFormError('')
    try {
      const payload: UserKeyPayload = {
        provider: form.provider,
        label: form.label.trim(),
        api_key: form.apiKey.trim() || undefined,
        base_url: form.baseUrl.trim(),
        models: form.models,
      }
      if (editing) {
        await updateUserKey(editing.id, payload)
      } else {
        await createUserKey(payload)
      }
      setFormOpen(false)
      await load()
    } catch (e) {
      setFormError(e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  async function confirmRemove() {
    if (!removing) return
    try {
      await deleteUserKey(removing.id)
      setRemoving(null)
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : '删除失败')
      setRemoving(null)
    }
  }

  return (
    <Card>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-[15px] font-semibold text-ink">自备密钥</h2>
          <p className="mt-0.5 text-[13px] text-ink-3">
            用你自己的上游额度调用模型，费用由上游直接收取，本站不重复扣费
          </p>
        </div>
        <Button
          variant="primary"
          size="sm"
          onClick={openCreate}
          disabled={providers.length === 0}
        >
          添加密钥
        </Button>
      </div>

      {loading && <SkeletonRows rows={2} />}

      {!loading && error && (
        <div className="rounded-md border border-err/25 bg-err/5 px-3 py-2.5 text-[13px] text-err">
          {error}
        </div>
      )}

      {!loading && !error && keys.length === 0 && (
        <EmptyState
          title="还没有配置自备密钥"
          description="配置后，你可以用自己的上游额度访问模型，调用仍走本站的协议、审计与统计。"
        />
      )}

      {!loading && !error && keys.length > 0 && (
        <ul className="space-y-2.5">
          {keys.map((key) => (
            <li
              key={key.id}
              className="flex flex-wrap items-start justify-between gap-3 rounded-md border border-line p-3.5"
            >
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-1.5">
                  <span className="text-[13px] font-semibold text-ink">
                    {key.provider_label}
                  </span>
                  {key.label && <span className="text-xs text-ink-3">{key.label}</span>}
                  {/* 熔断用警示色：它是平台侧的临时隔离，需要用户等待而非操作 */}
                  {key.cooling_down ? (
                    <Badge tone="warn">{key.status_text}</Badge>
                  ) : (
                    <Badge tone={key.status === 1 ? 'ok' : 'off'}>{key.status_text}</Badge>
                  )}
                </div>
                <div className="mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-ink-3">
                  {/* 只有掩码——服务端不会、也不应回传明文 */}
                  {key.masked_key && (
                    <span className="font-mono">{key.masked_key}</span>
                  )}
                  <span className="truncate" title={key.effective_base_url}>
                    {key.effective_base_url}
                  </span>
                  {key.models.length > 0 && (
                    <span>限定 {key.models.length} 个模型</span>
                  )}
                </div>
              </div>
              <div className="flex shrink-0 gap-1.5">
                <Button size="sm" variant="secondary" onClick={() => openEdit(key)}>
                  编辑
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setRemoving(key)}>
                  删除
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}

      {/* ── 新增 / 编辑弹层 ── */}
      <Modal
        open={formOpen}
        onClose={() => setFormOpen(false)}
        title={editing ? '编辑自备密钥' : '添加自备密钥'}
        footer={
          <>
            <Button variant="ghost" onClick={() => setFormOpen(false)}>
              取消
            </Button>
            <Button variant="primary" onClick={submit} loading={saving}>
              {editing ? '保存' : '添加'}
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <Field label="上游" required htmlFor="byok-provider">
            <Select
              id="byok-provider"
              value={form.provider}
              onChange={(e) => setForm({ ...form, provider: e.target.value, baseUrl: '' })}
            >
              <option value="">请选择</option>
              {providers.map((p) => (
                <option key={p.key} value={p.key}>
                  {p.label}
                </option>
              ))}
            </Select>
          </Field>

          {selectedProvider?.notes && (
            <p className="rounded-md border border-line bg-surface px-3 py-2 text-xs leading-relaxed text-ink-2">
              {selectedProvider.notes}
            </p>
          )}

          <Field
            label="API Key"
            required={!editing}
            htmlFor="byok-key"
            help={
              editing
                ? '留空表示不修改。出于安全考虑，平台不会回显已有密钥。'
                : '密钥将以 AES-256-GCM 加密后存储，界面上只显示掩码。'
            }
          >
            <Input
              id="byok-key"
              type="password"
              autoComplete="off"
              placeholder={editing ? '留空表示不修改' : '粘贴你的 API Key'}
              value={form.apiKey}
              onChange={(e) => setForm({ ...form, apiKey: e.target.value })}
            />
          </Field>

          <Field label="备注" htmlFor="byok-label" help="仅自己可见，便于分辨多把密钥">
            <Input
              id="byok-label"
              placeholder="如：生产环境"
              value={form.label}
              onChange={(e) => setForm({ ...form, label: e.target.value })}
            />
          </Field>

          <Field
            label="接入点"
            htmlFor="byok-base-url"
            help={
              selectedProvider?.base_url_editable
                ? '留空则使用该上游的官方地址'
                : '该上游不支持自定义接入点'
            }
          >
            <Input
              id="byok-base-url"
              placeholder={selectedProvider?.default_base_url}
              disabled={!selectedProvider?.base_url_editable}
              value={form.baseUrl}
              onChange={(e) => setForm({ ...form, baseUrl: e.target.value })}
            />
          </Field>

          <Field
            label="可用模型"
            htmlFor="byok-models"
            help="留空表示不限制；多个模型用逗号分隔"
          >
            <Input
              id="byok-models"
              placeholder="留空 = 该上游全部模型"
              value={form.models}
              onChange={(e) => setForm({ ...form, models: e.target.value })}
            />
            {/* 建议模型做成可点标签：省去用户去上游文档查模型名的来回，
                但只是提示而非白名单——实际能调什么以上游账号为准。 */}
            {selectedProvider?.suggested_models &&
              selectedProvider.suggested_models.length > 0 && (
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {selectedProvider.suggested_models.map((m) => (
                    <button
                      key={m}
                      type="button"
                      onClick={() => {
                        const current = form.models
                          .split(',')
                          .map((s) => s.trim())
                          .filter(Boolean)
                        if (current.includes(m)) return
                        setForm({ ...form, models: [...current, m].join(', ') })
                      }}
                      className="rounded border border-line bg-surface px-1.5 py-0.5 font-mono text-[11px] text-ink-3 hover:border-line-2 hover:text-ink-2"
                    >
                      + {m}
                    </button>
                  ))}
                </div>
              )}
          </Field>

          {formError && (
            <div className="rounded-md border border-err/25 bg-err/5 px-3 py-2 text-[13px] text-err">
              {formError}
            </div>
          )}
        </div>
      </Modal>

      {/* ── 删除二次确认 ── */}
      <Modal
        open={removing !== null}
        onClose={() => setRemoving(null)}
        title="删除自备密钥"
        width={420}
        footer={
          <>
            <Button variant="ghost" onClick={() => setRemoving(null)}>
              取消
            </Button>
            <Button variant="danger" onClick={confirmRemove}>
              确认删除
            </Button>
          </>
        }
      >
        <p className="text-[13px] leading-relaxed text-ink-2">
          确认删除
          <span className="font-medium text-ink">
            {removing?.provider_label}
            {removing?.label ? `（${removing.label}）` : ''}
          </span>
          ？
          凭据本就保存在你手里，删除后平台将不再使用它；该操作不可撤销。
        </p>
      </Modal>
    </Card>
  )
}
