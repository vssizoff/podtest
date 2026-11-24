<script setup lang="ts">
defineProps<{ label: string; modelValue: string; type?: string; rows?: number; placeholder?: string }>()
defineEmits<{ 'update:modelValue': [value: string] }>()
</script>
<template><label class="term-field"><span>{{ label }}</span><textarea v-if="rows" :rows="rows" :value="modelValue" :placeholder="placeholder" @input="$emit('update:modelValue', ($event.target as HTMLTextAreaElement).value)" /><input v-else :type="type ?? 'text'" :value="modelValue" :placeholder="placeholder" @input="$emit('update:modelValue', ($event.target as HTMLInputElement).value)" /></label></template>
