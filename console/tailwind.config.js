/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{vue,js}'],
  theme: {
    extend: {
      colors: {
        // 深色主题底色：冷调深石板蓝（crony 控制台自用配色）
        navy: {
          950: '#05080f',
          900: '#0a0f19',
          850: '#0e1522',
          800: '#121a29',
          750: '#151e2e',
          700: '#1a2436',
          600: '#223046',
          500: '#2f4055',
        },
        // 主强调色：青（cyan）——调度/运行态的冷色
        brand: {
          50: '#ecfeff',
          200: '#a5f3fc',
          300: '#67e8f9',
          400: '#22d3ee',
          500: '#06b6d4',
          600: '#0891b2',
          700: '#0e7490',
          800: '#155e75',
        },
        // 次要强调色（蓝）：与主色搭配用于渐变/对比标签
        violet2: '#3b82f6',
        ok: '#22c55e',
        warn: '#f59e0b',
        danger: '#ef4444',
        info: '#60a5fa',
        ink: {
          DEFAULT: '#e6ebf7',
          muted: '#a7b1cc',
          dim: '#6f7b9c',
        },
      },
      width: {
        sidebar: '245px',
        'sidebar-collapsed': '68px',
      },
      minWidth: {
        sidebar: '245px',
        'sidebar-collapsed': '68px',
      },
      transitionProperty: {
        width: 'width',
      },
      boxShadow: {
        glow: '0 0 0 1px rgba(6,182,212,0.35), 0 8px 30px rgba(6,182,212,0.28)',
        card: '0 1px 2px rgba(0,0,0,0.35), 0 12px 28px -18px rgba(0,0,0,0.8)',
      },
      keyframes: {
        'pulse-ring': {
          '0%': { transform: 'scale(0.9)', opacity: '0.7' },
          '70%': { transform: 'scale(1.6)', opacity: '0' },
          '100%': { transform: 'scale(1.6)', opacity: '0' },
        },
        'fade-slide-in': {
          '0%': { opacity: '0', transform: 'translateY(8px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
      },
      animation: {
        'pulse-ring': 'pulse-ring 2s cubic-bezier(0.4,0,0.6,1) infinite',
        'fade-slide-in': 'fade-slide-in 0.25s ease-out both',
      },
    },
  },
  plugins: [],
}