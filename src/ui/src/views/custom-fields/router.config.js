import Meta from '@/router/meta'
import { U_MODEL } from '@/dictionary/auth'
import { MENU_BUSINESS_ADVANCED } from '@/dictionary/menu-symbol'

export const OPERATION = { U_MODEL }

export default {
    name: 'customFields',
    path: 'custom-fields',
    component: () => import('./index.vue'),
    meta: new Meta({
        menu: {
            i18n: '自定义字段',
            parent: MENU_BUSINESS_ADVANCED
        },
        auth: {
            operation: { U_MODEL },
            authScope: 'business'
        }
    })
}