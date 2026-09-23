<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'

const props = defineProps<{ modelValue: boolean; title: string; busy?: boolean }>()
const emit = defineEmits<{ (e: 'update:modelValue', value: boolean): void }>()
const dialog = ref<HTMLDialogElement | null>(null)
let previousFocus: HTMLElement | null = null

watch(() => props.modelValue, async (open) => {
  if (!open) return
  previousFocus = document.activeElement as HTMLElement | null
  await nextTick()
  dialog.value?.showModal()
})

function dismiss() {
  if (!props.busy) emit('update:modelValue', false)
}

function afterLeave(el: Element) {
  ;(el as HTMLDialogElement).close()
  previousFocus?.focus()
}
</script>

<template>
  <Teleport to="body">
    <Transition name="dialog" @after-leave="afterLeave">
      <dialog v-if="modelValue" ref="dialog" :aria-label="title" @cancel.prevent="dismiss"
        @click="(e) => { if (e.target === dialog) dismiss() }">
        <section class="dialog-body">
          <h2>{{ title }}</h2>
          <slot />
        </section>
      </dialog>
    </Transition>
  </Teleport>
</template>

<style scoped>
dialog { width: min(440px, calc(100vw - 32px)); padding: 0; border: 1px solid #d8d2c7; border-radius: 12px; color: #242320; background: #faf8f3; box-shadow: 0 18px 60px #0002; }
dialog::backdrop { background: #24232040; }
.dialog-body { padding: 24px; }
h2 { margin: 0 0 18px; font-size: 1.1rem; }
.dialog-enter-active, .dialog-leave-active { transition: opacity 180ms cubic-bezier(.22,1,.36,1), transform 180ms cubic-bezier(.22,1,.36,1); }
.dialog-enter-active::backdrop, .dialog-leave-active::backdrop { transition: opacity 180ms ease-out; }
.dialog-enter-from, .dialog-leave-to { opacity: 0; transform: translateY(8px) scale(.98); }
.dialog-enter-from::backdrop, .dialog-leave-to::backdrop { opacity: 0; }
@media (prefers-reduced-motion: reduce) { .dialog-enter-active, .dialog-leave-active, .dialog-enter-active::backdrop, .dialog-leave-active::backdrop { transition: none; } }
</style>
