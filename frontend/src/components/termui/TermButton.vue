<script setup lang="ts">
withDefaults(defineProps<{ disabled?: boolean; type?: 'button' | 'submit' | 'reset'; variant?: 'primary' | 'plain' | 'danger' }>(), { disabled: false, type: 'button', variant: 'plain' })
</script>
<template><button class="term-button" :class="`term-button--${variant}`" :type="type" :disabled="disabled"><span><slot /></span></button></template>
