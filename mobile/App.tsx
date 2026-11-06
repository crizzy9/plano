import { useEffect, useMemo, useState } from 'react';
import { DarkTheme, DefaultTheme, NavigationContainer } from '@react-navigation/native';
import { createNativeStackNavigator } from '@react-navigation/native-stack';
import { StatusBar } from 'expo-status-bar';
import { AppState, Pressable, StyleSheet, Text, View } from 'react-native';
import { SafeAreaProvider } from 'react-native-safe-area-context';
import { createAPI, loadServer, removeRetiredDevices } from './src/api';
import { errorText } from './src/flight';
import { Home, Search, FlightDetailScreen, ServerSettings, type Routes } from './src/screens';
import { Tools, ImportFlights, PassportScreen, ConnectionScreen } from './src/tools';
import { usePalette } from './src/theme';
import { Loading } from './src/ui';

const Stack = createNativeStackNavigator<Routes>();

export default function App() {
  const { colors, isDark } = usePalette();
  const [server, setServer] = useState<string>();
  const [loadError, setLoadError] = useState<string>();
  useEffect(() => {
    loadServer().then(setServer, (error: unknown) => {
      setLoadError(errorText(error));
      setServer('');
    });
  }, []);
  const api = useMemo(() => createAPI(server ?? ''), [server]);
  useEffect(() => {
    if (!server) return;
    const clean = () => { void removeRetiredDevices(server).catch(() => {}); };
    clean();
    const subscription = AppState.addEventListener('change', (state) => {
      if (state === 'active') clean();
    });
    return () => subscription.remove();
  }, [server]);
  const baseTheme = isDark ? DarkTheme : DefaultTheme;
  const theme = {
    ...baseTheme,
    colors: {
      ...baseTheme.colors, background: colors.ground, card: colors.surface,
      text: colors.text, border: colors.divider, primary: colors.tint,
    },
  };

  return (
    <SafeAreaProvider>
      <StatusBar style={isDark ? 'light' : 'dark'} />
      {server === undefined ? (
        <View style={[styles.wait, { backgroundColor: colors.ground }]}><Loading /></View>
      ) : (
        <NavigationContainer key={server} theme={theme}>
          <Stack.Navigator screenOptions={{
            headerShadowVisible: false, headerBackButtonDisplayMode: 'minimal',
            contentStyle: { backgroundColor: colors.ground },
          }}>
            <Stack.Screen name="Home" options={({ navigation }) => ({
              title: 'Plano',
              headerRight: () => (
                <Pressable accessibilityRole="button" onPress={() => navigation.navigate('Settings')}
                  style={styles.headerAction}>
                  <Text style={{ color: colors.tint, fontSize: 15, fontWeight: '600' }}>Settings</Text>
                </Pressable>
              ),
            })}>
              {(props) => <Home {...props} api={api} />}
            </Stack.Screen>
            <Stack.Screen name="Search" options={{ title: 'Find a flight' }}>
              {(props) => <Search {...props} api={api} />}
            </Stack.Screen>
            <Stack.Screen name="Flight" options={({ route }) => ({ title: route.params.number })}>
              {(props) => <FlightDetailScreen {...props} api={api} />}
            </Stack.Screen>
            <Stack.Screen name="Tools" component={Tools} options={{ title: 'Trip tools' }} />
            <Stack.Screen name="Import" options={{ title: 'Import flights' }}>
              {(props) => <ImportFlights {...props} api={api} />}
            </Stack.Screen>
            <Stack.Screen name="Passport" options={{ title: 'Passport' }}>
              {(props) => <PassportScreen {...props} api={api} />}
            </Stack.Screen>
            <Stack.Screen name="Connection" options={{ title: 'Connection assistant' }}>
              {() => <ConnectionScreen api={api} />}
            </Stack.Screen>
            <Stack.Screen name="Settings" options={{ title: 'Settings' }}>
              {() => <ServerSettings api={api} error={loadError} onChange={(next) => {
                setLoadError(undefined);
                setServer(next);
              }} />}
            </Stack.Screen>
          </Stack.Navigator>
        </NavigationContainer>
      )}
    </SafeAreaProvider>
  );
}

const styles = StyleSheet.create({
  wait: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  headerAction: { minHeight: 48, minWidth: 48, justifyContent: 'center', paddingHorizontal: 4 },
});
