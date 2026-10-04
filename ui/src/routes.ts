import type { RouteRecordRaw } from 'vue-router'
import '@/main.css'

// Routes mounted by the platform shell; paths and permissions match
// pkg/smsgwmanifest Nav, and the server checks every call again.
const module = 'sms-gw'
export const routes: RouteRecordRaw[] = [
  { path: '/sms-gw/providers', name: 'sms-gw-providers', component: () => import('@/pages/Providers.vue'), meta: { module, requires: 'providers:read' } },
  { path: '/sms-gw/templates', name: 'sms-gw-templates', component: () => import('@/pages/Templates.vue'), meta: { module, requires: 'templates:read' } },
  { path: '/sms-gw/api-clients', name: 'sms-gw-api-clients', component: () => import('@/pages/ApiClients.vue'), meta: { module, requires: 'clients:read' } },
  { path: '/sms-gw/blocks', name: 'sms-gw-blocks', component: () => import('@/pages/Blocks.vue'), meta: { module, requires: 'blocks:read' } },
  { path: '/sms-gw/messages', name: 'sms-gw-messages', component: () => import('@/pages/Messages.vue'), meta: { module, requires: 'messages:read' } },
  { path: '/sms-gw/dashboard', name: 'sms-gw-dashboard', component: () => import('@/pages/Dashboard.vue'), meta: { module, requires: 'dashboard:read' } },
]
export default routes
