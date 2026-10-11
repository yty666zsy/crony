import { createApp } from 'vue'
import { createPinia } from 'pinia'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import * as ElementPlusIcons from '@element-plus/icons-vue'

// 样式引入顺序（重要）：
//   1) Element Plus 基础样式
//   2) Element Plus 深色 css-vars（依赖 html.dark）
//   3) 本工程主题：Tailwind 三层 + Element Plus 深色覆写（必须最后，才能覆盖组件默认值）
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import './styles/index.css'

import App from './App.vue'
import router from './router'

const app = createApp(App)

// 全量注册 Element Plus 图标（模板里可直接写 <el-icon><Monitor /></el-icon>）
for (const [name, component] of Object.entries(ElementPlusIcons)) {
  app.component(name, component)
}

app.use(createPinia())
app.use(router)
app.use(ElementPlus, { locale: zhCn })

app.mount('#app')