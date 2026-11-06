import { useColorScheme } from 'react-native';

const light = {
  ground: '#F5F7F4', surface: '#FFFFFF', text: '#132522', muted: '#435C56',
  tint: '#006952', onTint: '#FFFFFF', divider: '#CFDBD4', tintSoft: '#D8EFE5',
  alert: '#9B350F', alertSoft: '#FCE7DA', route: '#0F3C35', routeText: '#F1FBF5',
  routeMuted: '#B9DAD0', routeLine: '#82DDBD',
};

const dark: typeof light = {
  ground: '#111B19', surface: '#182724', text: '#EDF6F1', muted: '#B2C8C0',
  tint: '#6CE0B8', onTint: '#102B24', divider: '#3D524B', tintSoft: '#21473D',
  alert: '#FFBB9D', alertSoft: '#563528', route: '#153C34', routeText: '#F1FBF5',
  routeMuted: '#B9DAD0', routeLine: '#82DDBD',
};

export type Colors = typeof light;

export function usePalette() {
  const isDark = useColorScheme() === 'dark';
  return { isDark, colors: isDark ? dark : light };
}
