/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{vue,js}'],
  theme: {
    extend: {
      colors: {
        // 深色海军蓝主题（Baseline：参照 ITOps Agent 演示站观测值）
        navy: {
          950: '#080d1a',
          900: '#0b1020',
          850: '#101736',
          800: '#131a2e',
          750: '#161f38',
          700: '#1b2544',
          600: '#232e52',
          500: '#2c3a63',
        },
        brand: {
          50: '#eef0ff',
          200: '#c7d2fe',
          300: '#a5b4fc',
          400: '#818cf8',
          500: '#6366f1',
          600: '#5b5bd6',
          700: '#4f46e5',
          800: '#4338ca',
        },
        violet2: '#8b5cf6',
        ok: '#22c55e',
        warn: '#f59e0b',
        danger: '#ef4444',
        info: '#38bdf8',
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
        glow: '0 0 0 1px rgba(99,102,241,0.35), 0 8px 30px rgba(99,102,241,0.28)',
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